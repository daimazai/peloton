package offerpool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/uber-go/tally"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	sched "code.uber.internal/infra/peloton/.gen/mesos/v1/scheduler"

	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"

	"code.uber.internal/infra/peloton/hostmgr/scalar"
	"code.uber.internal/infra/peloton/hostmgr/summary"
	hostmgr_summary_mocks "code.uber.internal/infra/peloton/hostmgr/summary/mocks"
	"code.uber.internal/infra/peloton/util"
	mpb_mocks "code.uber.internal/infra/peloton/yarpc/encoding/mpb/mocks"
)

type mockJSONClient struct {
	rejectedOfferIds map[string]bool
}

func (c *mockJSONClient) Call(mesosStreamID string, msg proto.Message) error {
	call := msg.(*sched.Call)
	for _, id := range call.Decline.OfferIds {
		c.rejectedOfferIds[*id.Value] = true
	}
	return nil
}

type mockMesosStreamIDProvider struct {
}

func (msp *mockMesosStreamIDProvider) GetMesosStreamID(ctx context.Context) string {
	return "stream"
}

func (msp *mockMesosStreamIDProvider) GetFrameworkID(ctx context.Context) *mesos.FrameworkID {
	return nil
}

const (
	_perHostCPU = 10.0
	_perHostMem = 20.0
	pelotonRole = "peloton"
)

var (
	_testAgent   = "agent"
	_testOfferID = "testOffer"
	_testKey     = "testKey"
	_testValue   = "testValue"
)

func TestRemoveExpiredOffers(t *testing.T) {
	// empty offer pool
	scope := tally.NewTestScope("", map[string]string{})
	pool := &offerPool{
		timedOffers:     make(map[string]*TimedOffer),
		hostOfferIndex:  make(map[string]summary.HostSummary),
		timedOffersLock: &sync.Mutex{},
		offerHoldTime:   1 * time.Minute,
		metrics:         NewMetrics(scope),
	}
	removed, valid := pool.RemoveExpiredOffers()
	assert.Equal(t, len(removed), 0)
	assert.Equal(t, 0, valid)

	hostName1 := "agent1"
	offerID1 := "offer1"
	offer1 := getMesosOffer(hostName1, offerID1)

	hostName2 := "agent2"
	offerID2 := "offer2"
	offer2 := getMesosOffer(hostName2, offerID2)

	offerID3 := "offer3"
	offer3 := getMesosOffer(hostName1, offerID3)

	hostName4 := "agent4"
	offerID4 := "offer4"
	offer4 := getMesosOffer(hostName4, offerID4)

	// pool with offers within timeout
	pool.AddOffers(context.Background(), []*mesos.Offer{offer1, offer2, offer3, offer4})
	removed, valid = pool.RemoveExpiredOffers()
	assert.Empty(t, removed)
	assert.Equal(t, 4, valid)

	// adjust the time stamp
	pool.timedOffers[offerID1].Expiration = time.Now().Add(-2 * time.Minute)
	pool.timedOffers[offerID4].Expiration = time.Now().Add(-2 * time.Minute)

	expected := map[string]*TimedOffer{
		offerID1: pool.timedOffers[offerID1],
		offerID4: pool.timedOffers[offerID4],
	}

	removed, valid = pool.RemoveExpiredOffers()
	assert.Exactly(t, expected, removed)
	assert.Equal(t, 2, valid)

	/*
	   assert.Equal(t, 1, len(pool.hostOfferIndex[hostName1].unreservedOffers))
	   offer := pool.hostOfferIndex[hostName1].unreservedOffers[offerID3]
	   assert.Equal(t, offerID3, *offer.Id.Value)
	   assert.Empty(t, len(pool.hostOfferIndex[hostName4].unreservedOffers))
	*/
}

func getMesosOffer(hostName string, offerID string) *mesos.Offer {
	agentID := fmt.Sprintf("%s-%d", hostName, 1)
	return &mesos.Offer{
		Id: &mesos.OfferID{
			Value: &offerID,
		},
		AgentId: &mesos.AgentID{
			Value: &agentID,
		},
		Hostname: &hostName,
	}
}

func TestAddGetRemoveOffers(t *testing.T) {
	scope := tally.NewTestScope("", map[string]string{})
	pool := &offerPool{
		timedOffers:     make(map[string]*TimedOffer),
		hostOfferIndex:  make(map[string]summary.HostSummary),
		timedOffersLock: &sync.Mutex{},
		metrics:         NewMetrics(scope),
	}
	// Add offer concurrently
	nOffers := 10
	nAgents := 10
	wg := sync.WaitGroup{}
	wg.Add(nOffers)

	for i := 0; i < nOffers; i++ {
		go func(i int) {
			var offers []*mesos.Offer
			for j := 0; j < nAgents; j++ {
				hostName := fmt.Sprintf("agent-%d", j)
				offerID := fmt.Sprintf("%s-%d", hostName, i)
				offer := getMesosOffer(hostName, offerID)
				offers = append(offers, offer)
			}
			pool.AddOffers(context.Background(), offers)
			wg.Done()
		}(i)
	}
	wg.Wait()

	assert.Equal(t, nOffers*nAgents, len(pool.timedOffers))
	for i := 0; i < nOffers; i++ {
		for j := 0; j < nAgents; j++ {
			hostName := fmt.Sprintf("agent-%d", j)
			offerID := fmt.Sprintf("%s-%d", hostName, i)
			assert.Equal(t, pool.timedOffers[offerID].Hostname, hostName)
		}
	}
	for j := 0; j < nAgents; j++ {
		hostName := fmt.Sprintf("agent-%d", j)
		assert.True(t, pool.hostOfferIndex[hostName].HasOffer())
	}

	// Get offer for placement
	takenHostOffers := map[string][]*mesos.Offer{}
	mutex := &sync.Mutex{}
	nClients := 4
	var limit uint32 = 2
	wg = sync.WaitGroup{}
	wg.Add(nClients)
	for i := 0; i < nClients; i++ {
		go func(i int) {
			filter := &hostsvc.HostFilter{
				Quantity: &hostsvc.QuantityControl{
					MaxHosts: limit,
				},
			}
			hostOffers, _, err := pool.ClaimForPlace(filter)
			assert.NoError(t, err)
			assert.Equal(t, int(limit), len(hostOffers))
			mutex.Lock()
			defer mutex.Unlock()
			for hostname, offers := range hostOffers {
				assert.Equal(
					t,
					nOffers,
					len(offers),
					"hostname %s has incorrect offer length",
					hostname)
				if _, ok := takenHostOffers[hostname]; ok {
					assert.Fail(t, "Host %s is taken multiple times", hostname)
				}
				takenHostOffers[hostname] = offers
			}
			wg.Done()
		}(i)
	}
	wg.Wait()

	for hostname, offers := range takenHostOffers {
		s, ok := pool.hostOfferIndex[hostname]
		assert.True(t, ok)
		assert.NotNil(t, s)

		for _, offer := range offers {
			offerID := offer.GetId().GetValue()
			// Check that all offers are still around.
			assert.NotNil(t, pool.timedOffers[offerID])
		}
	}

	assert.Equal(t, nOffers*nAgents, len(pool.timedOffers))

	// Rescind all offers.
	wg = sync.WaitGroup{}
	wg.Add(nOffers)
	for i := 0; i < nOffers; i++ {
		go func(i int) {
			for j := 0; j < nAgents; j++ {
				hostName := fmt.Sprintf("agent-%d", j)
				offerID := fmt.Sprintf("%s-%d", hostName, i)
				rFound := pool.RescindOffer(&mesos.OfferID{Value: &offerID})
				assert.Equal(
					t,
					true,
					rFound,
					"Offer %s has inconsistent result when rescinding",
					offerID)
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	assert.Equal(t, len(pool.timedOffers), 0)
}

func TestResetExpiredHostSummaries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	type mockHelper struct {
		mockResetExpiredPlacingOfferStatus bool
		hostname                           string
	}

	testTable := []struct {
		helpers                 []mockHelper
		expectedPrunedHostnames []string
		msg                     string
	}{
		{
			helpers:                 []mockHelper{},
			expectedPrunedHostnames: []string{},
			msg: "Pool with no host",
		}, {
			helpers: []mockHelper{
				{
					mockResetExpiredPlacingOfferStatus: false,
					hostname: "host0",
				},
			},
			expectedPrunedHostnames: []string{},
			msg: "Pool with 1 host, 0 pruned",
		}, {
			helpers: []mockHelper{
				{
					mockResetExpiredPlacingOfferStatus: false,
					hostname: "host0",
				},
				{
					mockResetExpiredPlacingOfferStatus: true,
					hostname: "host1",
				},
			},
			expectedPrunedHostnames: []string{"host1"},
			msg: "Pool with 2 hosts, 1 pruned",
		}, {
			helpers: []mockHelper{
				{
					mockResetExpiredPlacingOfferStatus: true,
					hostname: "host0",
				},
				{
					mockResetExpiredPlacingOfferStatus: true,
					hostname: "host1",
				},
			},
			expectedPrunedHostnames: []string{"host0", "host1"},
			msg: "Pool with 2 hosts, 2 pruned",
		},
	}

	now := time.Now()
	scope := tally.NewTestScope("", map[string]string{})

	for _, tt := range testTable {
		hostOfferIndex := make(map[string]summary.HostSummary)
		for _, helper := range tt.helpers {
			mhs := hostmgr_summary_mocks.NewMockHostSummary(ctrl)
			mhs.EXPECT().ResetExpiredPlacingOfferStatus(now).Return(helper.mockResetExpiredPlacingOfferStatus, scalar.Resources{})
			hostOfferIndex[helper.hostname] = mhs
		}
		pool := &offerPool{
			hostOfferIndex: hostOfferIndex,
			metrics:        NewMetrics(scope),
		}
		resetHostnames := pool.ResetExpiredHostSummaries(now)
		assert.Equal(t, len(tt.expectedPrunedHostnames), len(resetHostnames), tt.msg)
		for _, hostname := range resetHostnames {
			assert.Contains(t, tt.expectedPrunedHostnames, hostname)
		}
	}
}

func TestAddToBeUnreservedOffers(t *testing.T) {
	// empty offer pool
	scope := tally.NewTestScope("", map[string]string{})
	ctrl := gomock.NewController(t)
	mockSchedulerClient := mpb_mocks.NewMockSchedulerClient(ctrl)
	defer ctrl.Finish()
	pool := &offerPool{
		timedOffers:                make(map[string]*TimedOffer),
		hostOfferIndex:             make(map[string]summary.HostSummary),
		timedOffersLock:            &sync.Mutex{},
		offerHoldTime:              1 * time.Minute,
		metrics:                    NewMetrics(scope),
		mesosFrameworkInfoProvider: &mockMesosStreamIDProvider{},
		mSchedulerClient:           mockSchedulerClient,
	}

	mockSchedulerClient.EXPECT().
		Call(
			gomock.Eq("stream"),
			gomock.Any()).
		Do(func(_ string, msg proto.Message) {
			// Verify implicit reconcile call.
			call := msg.(*sched.Call)
			assert.Equal(t, sched.Call_ACCEPT, call.GetType())
			assert.Equal(t, "", call.GetFrameworkId().GetValue())
		}).
		Return(nil)

	reservation := &mesos.Resource_ReservationInfo{
		Labels: &mesos.Labels{
			Labels: []*mesos.Label{
				{
					Key:   &_testKey,
					Value: &_testValue,
				},
			},
		},
	}
	reservedResources := []*mesos.Resource{
		util.NewMesosResourceBuilder().
			WithName("cpus").
			WithValue(_perHostCPU).
			WithRole(pelotonRole).
			WithReservation(reservation).
			Build(),
		util.NewMesosResourceBuilder().
			WithName("mem").
			WithValue(_perHostMem).
			WithReservation(reservation).
			WithRole(pelotonRole).
			Build(),
	}

	offer1 := createReservedMesosOffer(reservedResources)

	pool.AddOffers(context.Background(), []*mesos.Offer{offer1})
	assert.Equal(t, int64(0), scope.Snapshot().Counters()["pool.call.unreserve+"].Value())
	pool.CleanReservationResources()
	assert.Equal(t, int64(1), scope.Snapshot().Counters()["pool.call.unreserve+"].Value())
	assert.Equal(t, int64(0), scope.Snapshot().Counters()["pool.fail.unreserve+"].Value())
}

func TestAddToBeUnreservedOffersFailure(t *testing.T) {
	// empty offer pool
	scope := tally.NewTestScope("", map[string]string{})
	ctrl := gomock.NewController(t)
	mockSchedulerClient := mpb_mocks.NewMockSchedulerClient(ctrl)
	defer ctrl.Finish()
	pool := &offerPool{
		timedOffers:                make(map[string]*TimedOffer),
		hostOfferIndex:             make(map[string]summary.HostSummary),
		timedOffersLock:            &sync.Mutex{},
		offerHoldTime:              1 * time.Minute,
		metrics:                    NewMetrics(scope),
		mesosFrameworkInfoProvider: &mockMesosStreamIDProvider{},
		mSchedulerClient:           mockSchedulerClient,
	}

	mockSchedulerClient.EXPECT().
		Call(
			gomock.Eq("stream"),
			gomock.Any()).
		Return(fmt.Errorf("some error"))

	reservation := &mesos.Resource_ReservationInfo{
		Labels: &mesos.Labels{
			Labels: []*mesos.Label{
				{
					Key:   &_testKey,
					Value: &_testValue,
				},
			},
		},
	}
	reservedResources := []*mesos.Resource{
		util.NewMesosResourceBuilder().
			WithName("cpus").
			WithValue(_perHostCPU).
			WithRole(pelotonRole).
			WithReservation(reservation).
			Build(),
		util.NewMesosResourceBuilder().
			WithName("mem").
			WithValue(_perHostMem).
			WithReservation(reservation).
			WithRole(pelotonRole).
			Build(),
	}

	offer1 := createReservedMesosOffer(reservedResources)

	pool.AddOffers(context.Background(), []*mesos.Offer{offer1})
	assert.Equal(t, int64(0), scope.Snapshot().Counters()["pool.call.unreserve+"].Value())
	pool.CleanReservationResources()
	assert.Equal(t, int64(0), scope.Snapshot().Counters()["pool.call.unreserve+"].Value())
	assert.Equal(t, int64(1), scope.Snapshot().Counters()["pool.fail.unreserve+"].Value())
}

func createReservedMesosOffer(res []*mesos.Resource) *mesos.Offer {
	return &mesos.Offer{
		Id: &mesos.OfferID{
			Value: &_testOfferID,
		},
		AgentId: &mesos.AgentID{
			Value: &_testAgent,
		},
		Hostname:  &_testAgent,
		Resources: res,
	}
}

// TODO: Add test case covering:
// - ready offer pruned;
// - ready offer rescinded;
// - ready offer claimed but never launch within expiration;
// - ready offer claimed then returnd unused;
// - ready offer claimed then launched;
// - launch w/ offer already expired/rescinded/pruned;
// - return offer already expired/rescinded/pruned.
