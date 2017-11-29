package offerpool

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/uber-go/atomic"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	sched "code.uber.internal/infra/peloton/.gen/mesos/v1/scheduler"
	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"

	"code.uber.internal/infra/peloton/common/constraints"
	common_scalar "code.uber.internal/infra/peloton/common/scalar"
	"code.uber.internal/infra/peloton/hostmgr/factory/operation"
	hostmgr_mesos "code.uber.internal/infra/peloton/hostmgr/mesos"
	"code.uber.internal/infra/peloton/hostmgr/scalar"
	"code.uber.internal/infra/peloton/hostmgr/summary"
	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/yarpc/encoding/mpb"
)

// Pool caches a set of offers received from Mesos master. It is
// currently only instantiated at the leader of Peloton masters.
type Pool interface {
	// Add offers to the pool
	AddOffers(context.Context, []*mesos.Offer)

	// Rescind a offer from the pool.
	// Returns whether the offer is found in the pool.
	RescindOffer(*mesos.OfferID) bool

	// Remove expired offers from the pool
	RemoveExpiredOffers() (map[string]*TimedOffer, int)

	// Clear offers in the pool
	Clear()

	// Decline offers
	DeclineOffers(ctx context.Context, offerIds []*mesos.OfferID) error

	// ClaimForPlace obtains offers from pool conforming to given HostFilter
	// for placement purposes.
	// First return value is returned offers, grouped by hostname as key,
	// Second return value is a map from hostsvc.HostFilterResult to count.
	ClaimForPlace(constraint *hostsvc.HostFilter) (
		map[string][]*mesos.Offer, map[string]uint32, error)

	// ClaimForLaunch finds offers previously for placement on given host.
	// The difference from ClaimForPlace is that offers claimed from this
	// function are considered used and sent back to Mesos master in a Launch
	// operation, while result in `ClaimForPlace` are still considered part
	// of peloton apps.
	ClaimForLaunch(hostname string, useReservedOffers bool) (map[string]*mesos.Offer, error)

	// ReturnUnusedOffers returns previously placed offers on hostname back
	// to current offer pool so they can be used by future launch actions.
	ReturnUnusedOffers(hostname string) error

	// TODO: Add following API for viewing offers, and optionally expose
	//  this in a debugging endpoint.
	// View() (map[string][]*mesos.Offer, err)

	// ResetExpiredHostSummaries resets the status of each hostSummary of the offerPool
	// from PlacingOffer to ReadyOffer if the PlacingOffer status has expired
	// and returns the hostnames which got reset
	ResetExpiredHostSummaries(now time.Time) []string

	// CleanReservationResources unreserves the offers without persistent volume.
	CleanReservationResources()
}

const (
	_defaultContextTimeout = 10 * time.Second
)

// NewOfferPool creates a offerPool object and registers the
// corresponding YARPC procedures.
func NewOfferPool(
	offerHoldTime time.Duration,
	schedulerClient mpb.SchedulerClient,
	metrics *Metrics,
	frameworkInfoProvider hostmgr_mesos.FrameworkInfoProvider,
	volumeStore storage.PersistentVolumeStore,
) Pool {
	p := &offerPool{
		hostOfferIndex: make(map[string]summary.HostSummary),

		timedOffers:     make(map[string]*TimedOffer),
		timedOffersLock: &sync.Mutex{},
		offerHoldTime:   offerHoldTime,

		mSchedulerClient:           schedulerClient,
		mesosFrameworkInfoProvider: frameworkInfoProvider,

		metrics: metrics,

		volumeStore: volumeStore,
	}

	// Initialize gauges.
	p.updateMetrics(p.readyResources.Get(), p.placingResources.Get())

	return p
}

// TimedOffer contains hostname and possible expiration time of an offer.
type TimedOffer struct {
	Hostname   string
	Expiration time.Time
}

type offerPool struct {
	sync.RWMutex

	// hostOfferIndex -- key: hostname, value: HostSummary
	hostOfferIndex map[string]summary.HostSummary

	// number of hosts that has any offers, i.e. both reserved and unreserved
	// offers. It includes both READY and PLACING state hosts.
	availableHosts atomic.Uint32

	// Map from id to hostname and expiration.
	// Used when offer is rescinded or pruned.
	timedOffers     map[string]*TimedOffer
	timedOffersLock *sync.Mutex

	// Time to hold offer for
	offerHoldTime time.Duration

	mSchedulerClient           mpb.SchedulerClient
	mesosFrameworkInfoProvider hostmgr_mesos.FrameworkInfoProvider

	metrics          *Metrics
	readyResources   scalar.AtomicResources
	placingResources scalar.AtomicResources

	volumeStore storage.PersistentVolumeStore
}

// ClaimForPlace obtains offers from pool conforming to given constraints.
// Results are grouped by hostname as key.
// This implements Pool.ClaimForPlace.
func (p *offerPool) ClaimForPlace(hostFilter *hostsvc.HostFilter) (
	map[string][]*mesos.Offer, map[string]uint32, error) {

	if hostFilter == nil {
		return nil, nil, errors.New("empty HostFilter passed in")
	}

	p.RLock()
	defer p.RUnlock()

	matcher := NewMatcher(
		hostFilter,
		constraints.NewEvaluator(task.LabelConstraint_HOST))

	for hostname, summary := range p.hostOfferIndex {
		matcher.tryMatch(hostname, summary)
		if matcher.HasEnoughHosts() {
			break
		}
	}

	hasEnoughHosts := matcher.HasEnoughHosts()

	hostOffers, resultCount := matcher.getHostOffers()

	var delta scalar.Resources
	for _, offers := range hostOffers {
		for _, offer := range offers {
			tmp := scalar.FromOffer(offer)
			delta = *(delta.Add(&tmp))
		}
	}
	if len(hostOffers) > 0 {
		incQuantity(&p.placingResources, delta, p.metrics.placing)
		decQuantity(&p.readyResources, delta, p.metrics.ready)
		log.WithFields(log.Fields{
			"host_filter":         hostFilter,
			"host_offers_noindex": hostOffers,
			"result_count":        resultCount,
			"delta":               delta,
			"placing_resources":   p.placingResources.Get(),
			"ready_resources":     p.readyResources.Get(),
		}).Debug("Claim offers for place.")
	}

	if !hasEnoughHosts {
		// Still proceed to return something.
		log.WithFields(log.Fields{
			"host_filter":                 hostFilter,
			"matched_host_offers_noindex": hostOffers,
			"match_result_counts":         resultCount,
		}).Debug("Not enough offers are matched to given constraints")
	}
	// NOTE: we should not clear the entries for the selected offers in p.offers
	// because we still need to visit corresponding offers, when these offers
	// are returned or used.
	return hostOffers, resultCount, nil
}

// ClaimForLaunch takes offers from pool for launch.
func (p *offerPool) ClaimForLaunch(hostname string, useReservedOffers bool) (
	map[string]*mesos.Offer, error) {
	// TODO: This is very similar to RemoveExpiredOffers, maybe refactor it.
	p.RLock()
	defer p.RUnlock()
	p.timedOffersLock.Lock()
	defer p.timedOffersLock.Unlock()

	var offerMap map[string]*mesos.Offer
	var err error
	if useReservedOffers {
		offerMap, err = p.hostOfferIndex[hostname].ClaimReservedOffersForLaunch()
	} else {
		offerMap, err = p.hostOfferIndex[hostname].ClaimForLaunch()
	}
	if err != nil {
		return nil, err
	}

	for id := range offerMap {
		if _, ok := p.timedOffers[id]; ok {
			delete(p.timedOffers, id)
		} else {
			log.WithFields(log.Fields{
				"offer_id": id,
				"host":     hostname,
			}).Warn("Offer id is not in pool")
		}
	}

	if !useReservedOffers {
		delta := scalar.FromOfferMap(offerMap)
		decQuantity(&p.placingResources, delta, p.metrics.placing)
		log.WithFields(log.Fields{
			"hostname":               hostname,
			"claimed_offers_noindex": offerMap,
			"delta":                  delta,
			"placing_resources":      p.placingResources.Get(),
			"ready_resources":        p.readyResources.Get(),
		}).Debug("Claiming offer for launch")
	}

	if !p.hostOfferIndex[hostname].HasAnyOffer() {
		p.metrics.AvailableHosts.Update(float64(p.availableHosts.Dec()))
	}

	return offerMap, nil
}

func (p *offerPool) updateMetrics(ready scalar.Resources, placing scalar.Resources) {
	p.metrics.ready.Update(&ready)
	p.metrics.placing.Update(&placing)
}

// tryAddOffer acquires read lock of the offerPool.
// If the offerPool does not have the hostName, returns false; otherwise,
// add offer with its expiration time to the agent and returns true.
func (p *offerPool) tryAddOffer(ctx context.Context, offer *mesos.Offer, expiration time.Time) bool {
	p.RLock()
	defer p.RUnlock()

	hostName := *offer.Hostname
	if _, ok := p.hostOfferIndex[hostName]; !ok {
		return false
	}
	if !p.hostOfferIndex[hostName].HasAnyOffer() {
		p.metrics.AvailableHosts.Update(float64(p.availableHosts.Inc()))
	}
	status := p.hostOfferIndex[hostName].AddMesosOffer(ctx, offer)

	delta := scalar.FromOffer(offer)
	switch status {
	case summary.ReadyOffer:
		incQuantity(&p.readyResources, delta, p.metrics.ready)
	case summary.PlacingOffer:
		incQuantity(&p.placingResources, delta, p.metrics.placing)
	default:
		log.WithField("status", status).
			Error("Unknown CacheStatus")
	}
	return true
}

// addOffer acquires the write lock. It would guarantee that the hostName
// correspond to the offer is added, then add the offer to the agent.
func (p *offerPool) addOffer(ctx context.Context, offer *mesos.Offer, expiration time.Time) {
	p.Lock()
	defer p.Unlock()

	hostName := *offer.Hostname
	_, ok := p.hostOfferIndex[hostName]
	if !ok {
		p.hostOfferIndex[hostName] = summary.New(p.volumeStore)
	}

	if !p.hostOfferIndex[hostName].HasAnyOffer() {
		p.metrics.AvailableHosts.Update(float64(p.availableHosts.Inc()))
	}

	status := p.hostOfferIndex[hostName].AddMesosOffer(ctx, offer)

	delta := scalar.FromOffer(offer)
	switch status {
	case summary.ReadyOffer:
		incQuantity(&p.readyResources, delta, p.metrics.ready)
	case summary.PlacingOffer:
		incQuantity(&p.placingResources, delta, p.metrics.placing)
	default:
		log.WithField("status", status).
			Error("Unknown CacheStatus")
	}
}

func (p *offerPool) AddOffers(ctx context.Context, offers []*mesos.Offer) {
	expiration := time.Now().Add(p.offerHoldTime)
	for _, offer := range offers {
		result := p.tryAddOffer(ctx, offer, expiration)
		if !result {
			p.addOffer(ctx, offer, expiration)
		}
	}

	p.timedOffersLock.Lock()
	defer p.timedOffersLock.Unlock()
	for _, offer := range offers {
		offerID := *offer.Id.Value
		p.timedOffers[offerID] = &TimedOffer{
			Hostname:   offer.GetHostname(),
			Expiration: expiration,
		}
	}
}

func (p *offerPool) RescindOffer(offerID *mesos.OfferID) bool {
	id := offerID.GetValue()
	log.WithField("offer_id", id).Debug("RescindOffer Received")
	p.RLock()
	defer p.RUnlock()
	p.timedOffersLock.Lock()
	defer p.timedOffersLock.Unlock()

	oID := *offerID.Value
	offer, ok := p.timedOffers[oID]
	if !ok {
		log.WithField("offer_id", offerID).Warn("OfferID not found in pool")
		return false
	}

	delete(p.timedOffers, oID)

	// Remove offer from hostOffers
	hostName := offer.Hostname
	hostOffers, ok := p.hostOfferIndex[hostName]
	if !ok {
		log.WithFields(log.Fields{
			"host":     hostName,
			"offer_id": id,
		}).Warn("host not found in hostOfferIndex")
		return false
	}

	status, removed := hostOffers.RemoveMesosOffer(oID)

	if removed != nil {
		delta := scalar.FromOffer(removed)
		switch status {
		case summary.ReadyOffer:
			decQuantity(&p.readyResources, delta, p.metrics.ready)
		case summary.PlacingOffer:
			decQuantity(&p.placingResources, delta, p.metrics.placing)
		default:
			log.WithField("status", status).
				Error("Unknown CacheStatus")
		}
		log.WithFields(log.Fields{
			"hostname":          hostName,
			"host_status":       status,
			"removed_offers":    removed,
			"delta":             delta,
			"placing_resources": p.placingResources.Get(),
			"ready_resources":   p.readyResources.Get(),
		}).Debug("Remove rescinded offer.")
	}

	if !p.hostOfferIndex[hostName].HasAnyOffer() {
		p.metrics.AvailableHosts.Update(float64(p.availableHosts.Dec()))
	}

	return true
}

// RemoveExpiredOffers removes offers which are expired from pool
// and return the list of removed mesos offer ids and their location as well as
// how many valid offers are still left.
func (p *offerPool) RemoveExpiredOffers() (map[string]*TimedOffer, int) {
	p.RLock()
	defer p.RUnlock()
	p.timedOffersLock.Lock()
	defer p.timedOffersLock.Unlock()

	offersToDecline := map[string]*TimedOffer{}

	for offerID, timedOffer := range p.timedOffers {
		if time.Now().After(timedOffer.Expiration) {
			log.WithField("offer_id", offerID).
				Debug("Removing expired offer from pool")
			offersToDecline[offerID] = timedOffer
			delete(p.timedOffers, offerID)
		}
	}

	// Remove the expired offers from hostOfferIndex
	if len(offersToDecline) > 0 {
		for offerID, offer := range offersToDecline {
			hostName := offer.Hostname
			status, removed := p.hostOfferIndex[hostName].RemoveMesosOffer(offerID)
			if removed != nil {
				delta := scalar.FromOffer(removed)
				switch status {
				case summary.ReadyOffer:
					decQuantity(&p.readyResources, delta, p.metrics.ready)
				case summary.PlacingOffer:
					decQuantity(&p.placingResources, delta, p.metrics.placing)
				default:
					log.WithField("status", status).
						Error("Unknown CacheStatus")
				}
				log.WithFields(log.Fields{
					"host":              hostName,
					"host_status":       status,
					"removed_offers":    removed,
					"delta":             delta,
					"placing_resources": p.placingResources.Get(),
					"ready_resources":   p.readyResources.Get(),
				}).Debug("remove expired offer.")
			}

			if !p.hostOfferIndex[hostName].HasAnyOffer() {
				p.metrics.AvailableHosts.Update(float64(p.availableHosts.Dec()))
			}
		}
	}
	return offersToDecline, len(p.timedOffers)
}

// Clear removes all offers from pool.
func (p *offerPool) Clear() {
	log.Info("Clean up offers")
	p.Lock()
	defer p.Unlock()
	p.timedOffersLock.Lock()
	defer p.timedOffersLock.Unlock()

	p.timedOffers = map[string]*TimedOffer{}
	p.hostOfferIndex = map[string]summary.HostSummary{}
	p.readyResources = scalar.AtomicResources{}
	p.placingResources = scalar.AtomicResources{}
	p.updateMetrics(p.readyResources.Get(), p.readyResources.Get())
}

// DeclineOffers calls mesos master to decline list of offers
func (p *offerPool) DeclineOffers(ctx context.Context, offerIDs []*mesos.OfferID) error {
	log.WithField("offer_ids", offerIDs).Debug("Decline offers")
	callType := sched.Call_DECLINE
	msg := &sched.Call{
		FrameworkId: p.mesosFrameworkInfoProvider.GetFrameworkID(ctx),
		Type:        &callType,
		Decline: &sched.Call_Decline{
			OfferIds: offerIDs,
		},
	}
	msid := p.mesosFrameworkInfoProvider.GetMesosStreamID(ctx)
	err := p.mSchedulerClient.Call(msid, msg)
	if err != nil {
		// Ideally, we assume that Mesos has offer_timeout configured,
		// so in the event that offer declining call fails, offers
		// should eventually be invalidated by Mesos.
		log.WithError(err).
			WithField("call", msg).
			Warn("Failed to decline offers")
		p.metrics.DeclineFail.Inc(1)
		return err
	}

	return nil
}

// ReturnUnusedOffers returns resources previously sent to placement engine
// back to ready state.
func (p *offerPool) ReturnUnusedOffers(hostname string) error {
	p.RLock()
	defer p.RUnlock()

	hostOffers, ok := p.hostOfferIndex[hostname]
	if !ok {
		log.WithField("host", hostname).
			Warn("Offers returned to pool but not found, maybe pruned?")
		return nil
	}

	err := hostOffers.CasStatus(summary.PlacingOffer, summary.ReadyOffer)
	if err != nil {
		return err
	}

	delta := hostOffers.UnreservedAmount()

	decQuantity(&p.placingResources, delta, p.metrics.placing)
	incQuantity(&p.readyResources, delta, p.metrics.ready)

	log.WithFields(log.Fields{
		"host":              hostname,
		"delta":             delta,
		"placing_resources": p.placingResources.Get(),
		"ready_resources":   p.readyResources.Get(),
	}).Debug("Returned offers to Ready state.")

	return nil
}

func incQuantity(
	resources *scalar.AtomicResources,
	delta scalar.Resources,
	gaugeMaps common_scalar.GaugeMaps,
) {
	tmp := resources.Get()
	tmp = *(tmp.Add(&delta))
	resources.Set(tmp)
	gaugeMaps.Update(&tmp)
}

func decQuantity(
	resources *scalar.AtomicResources,
	delta scalar.Resources,
	gaugeMaps common_scalar.GaugeMaps,
) {
	curr := resources.Get()
	if !(&curr).Contains(&delta) {
		// NOTE: we still proceed from there, but logs an error which
		// we hope to recover from logging.
		// This could be triggered by either missed offer tracking in
		// pool, or float point precision problem.
		log.WithFields(log.Fields{
			"current": curr,
			"delta":   delta,
		}).Error("Not sufficient resource to subtract delta!")
	}
	tmp := *(curr.Subtract(&delta))
	resources.Set(tmp)
	gaugeMaps.Update(&tmp)
}

// ResetExpiredHostSummaries resets the status of each hostSummary of the offerPool
// from PlacingOffer to ReadyOffer if the PlacingOffer status has expired
// and returns the hostnames which got reset
func (p *offerPool) ResetExpiredHostSummaries(now time.Time) []string {
	p.RLock()
	defer p.RUnlock()
	var resetHostnames []string
	for hostname, summary := range p.hostOfferIndex {
		if reset, res := summary.ResetExpiredPlacingOfferStatus(now); reset {
			resetHostnames = append(resetHostnames, hostname)
			incQuantity(&p.readyResources, res, p.metrics.ready)
			decQuantity(&p.placingResources, res, p.metrics.placing)
			log.WithFields(log.Fields{
				"host":              hostname,
				"summary":           summary,
				"delta":             res,
				"placing_resources": p.placingResources.Get(),
				"ready_resources":   p.readyResources.Get(),
			}).Debug("reset expired host summaries.")
		}
	}
	return resetHostnames
}

func (p *offerPool) CleanReservationResources() {
	p.RLock()
	defer p.RUnlock()
	p.metrics.CleanReservationResource.Inc(1)
	for hostname, summary := range p.hostOfferIndex {
		for _, offer := range summary.RemoveUnusedReservedOffers() {
			if err := p.unreserveOffer(offer); err != nil {
				p.metrics.UnreserveOfferFail.Inc(1)
				log.WithFields(log.Fields{
					"hostname": hostname,
					"offer":    offer,
				}).Error("Failed to unreserve unused offer resource")
				continue
			}
			p.metrics.UnreserveOffer.Inc(1)
			log.WithFields(log.Fields{
				"hostname": hostname,
				"offer":    offer,
			}).Info("Unreserve unused offer resource")
		}
	}
}

// unreserveOffer calls mesos master to unreserve resources that contain no
// persistent volume.
func (p *offerPool) unreserveOffer(offer *mesos.Offer) error {
	ctx, cancel := context.WithTimeout(context.Background(), _defaultContextTimeout)
	defer cancel()

	operations := []*hostsvc.OfferOperation{
		{
			Type: hostsvc.OfferOperation_UNRESERVE,
		},
	}
	factory := operation.NewOfferOperationsFactory(
		operations,
		offer.GetResources(),
		offer.GetHostname(),
		offer.GetAgentId(),
	)
	offerOperations, err := factory.GetOfferOperations()
	if err != nil {
		return err
	}

	callType := sched.Call_ACCEPT
	msg := &sched.Call{
		FrameworkId: p.mesosFrameworkInfoProvider.GetFrameworkID(ctx),
		Type:        &callType,
		Accept: &sched.Call_Accept{
			OfferIds:   []*mesos.OfferID{offer.GetId()},
			Operations: offerOperations,
		},
	}

	log.WithFields(log.Fields{
		"offer": offer,
		"call":  msg,
	}).Debug("unreserving offer with operations")

	msid := p.mesosFrameworkInfoProvider.GetMesosStreamID(ctx)
	return p.mSchedulerClient.Call(msid, msg)
}
