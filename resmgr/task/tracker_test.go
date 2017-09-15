package task

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/uber-go/tally"

	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	resp "code.uber.internal/infra/peloton/.gen/peloton/api/respool"
	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/common/eventstream"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"code.uber.internal/infra/peloton/resmgr/scalar"
)

type TrackerTestSuite struct {
	suite.Suite

	tracker            Tracker
	eventStreamHandler *eventstream.Handler
	task               *resmgr.Task
	respool            respool.ResPool
	hostname           string
}

func (suite *TrackerTestSuite) SetupTest() {
	InitTaskTracker(tally.NoopScope)
	suite.tracker = GetTracker()
	suite.eventStreamHandler = eventstream.NewEventStreamHandler(
		1000,
		[]string{
			common.PelotonJobManager,
			common.PelotonResourceManager,
		},
		nil,
		tally.Scope(tally.NoopScope))
	suite.hostname = "hostname"
	suite.task = suite.createTask(1)
	suite.addTasktotracker(suite.task)
}

func (suite *TrackerTestSuite) addTasktotracker(task *resmgr.Task) {
	rootID := peloton.ResourcePoolID{Value: respool.RootResPoolID}
	policy := resp.SchedulingPolicy_PriorityFIFO
	respoolConfig := &resp.ResourcePoolConfig{
		Name:      "respool-1",
		Parent:    &rootID,
		Resources: suite.getResourceConfig(),
		Policy:    policy,
	}
	suite.respool, _ = respool.NewRespool(tally.NoopScope, "respool-1", nil, respoolConfig)
	suite.tracker.AddTask(task, suite.eventStreamHandler, suite.respool, &Config{})
}

// Returns resource configs
func (suite *TrackerTestSuite) getResourceConfig() []*resp.ResourceConfig {

	resConfigs := []*resp.ResourceConfig{
		{
			Share:       1,
			Kind:        "cpu",
			Reservation: 100,
			Limit:       1000,
		},
		{
			Share:       1,
			Kind:        "memory",
			Reservation: 1000,
			Limit:       1000,
		},
		{
			Share:       1,
			Kind:        "disk",
			Reservation: 100,
			Limit:       1000,
		},
		{
			Share:       1,
			Kind:        "gpu",
			Reservation: 2,
			Limit:       4,
		},
	}
	return resConfigs
}

func (suite *TrackerTestSuite) createTask(instance int) *resmgr.Task {
	taskID := fmt.Sprintf("job1-%d", instance)
	return &resmgr.Task{
		Name:     taskID,
		Priority: 0,
		JobId:    &peloton.JobID{Value: "job1"},
		Id:       &peloton.TaskID{Value: taskID},
		Hostname: suite.hostname,
		Resource: &task.ResourceConfig{
			CpuLimit:    1,
			DiskLimitMb: 10,
			GpuLimit:    0,
			MemLimitMb:  100,
		},
	}
}

func TestTracker(t *testing.T) {
	suite.Run(t, new(TrackerTestSuite))
}

func (suite *TrackerTestSuite) TestTasksByHosts() {
	result := suite.tracker.TasksByHosts([]string{suite.hostname}, suite.task.Type)
	suite.Equal(1, len(result))
	suite.Equal(1, len(result[suite.hostname]))
	suite.Equal(suite.task, result[suite.hostname][0].task)
}

func (suite *TrackerTestSuite) TestTransition() {
	rmTask := suite.tracker.GetTask(suite.task.Id)
	err := rmTask.TransitTo(task.TaskState_PENDING.String())
	suite.NoError(err)
	err = rmTask.TransitTo(task.TaskState_READY.String())
	suite.NoError(err)
}

func (suite *TrackerTestSuite) TestSetPlacement() {
	oldHostname := suite.hostname
	for i := 0; i < 5; i++ {
		newHostname := fmt.Sprintf("new-hostname-%v", i)
		suite.tracker.SetPlacement(suite.task.Id, newHostname)

		result := suite.tracker.TasksByHosts([]string{newHostname}, suite.task.Type)
		suite.Equal(1, len(result))
		suite.Equal(1, len(result[newHostname]))
		suite.Equal(suite.task, result[newHostname][0].task)

		result = suite.tracker.TasksByHosts([]string{oldHostname}, suite.task.Type)
		suite.Equal(0, len(result))
	}
}

func (suite *TrackerTestSuite) TestSetPlacementHost() {
	suite.tracker.Clear()
	placement := &resmgr.Placement{}
	var tasks []*peloton.TaskID
	for i := 0; i < 5; i++ {
		taskID := fmt.Sprintf("job1-%d", i)
		task := &peloton.TaskID{Value: taskID}
		tasks = append(tasks, task)
		suite.addTasktotracker(suite.createTask(i))
	}
	placement.Tasks = tasks
	suite.tracker.SetPlacementHost(placement, suite.hostname)
	result := suite.tracker.TasksByHosts([]string{suite.hostname}, suite.task.Type)
	suite.Equal(5, len(result[suite.hostname]))
	suite.tracker.Clear()
}

func (suite *TrackerTestSuite) TestDelete() {
	suite.tracker.DeleteTask(suite.task.Id)
	rmTask := suite.tracker.GetTask(suite.task.Id)
	suite.Nil(rmTask)
	result := suite.tracker.TasksByHosts([]string{suite.hostname}, suite.task.Type)
	suite.Equal(0, len(result))
}

func (suite *TrackerTestSuite) TestClear() {
	suite.tracker.Clear()
	suite.Equal(suite.tracker.GetSize(), int64(0))
}

func (suite *TrackerTestSuite) TestAddResources() {
	res := suite.respool.GetAllocation()
	suite.Equal(res.GetCPU(), float64(0))
	suite.tracker.AddResources(&peloton.TaskID{Value: "job1-1"})
	res = suite.respool.GetAllocation()
	suite.Equal(res.GetCPU(), float64(1))
}

func (suite *TrackerTestSuite) TestGetTaskStates() {
	result := suite.tracker.GetActiveTasks("", "")
	suite.Equal(1, len(result))

	result = suite.tracker.GetActiveTasks("foo", "")
	suite.Equal(0, len(result))
}

func (suite *TrackerTestSuite) TestMarkItDone() {
	suite.tracker.Clear()
	for i := 0; i < 5; i++ {
		suite.addTasktotracker(suite.createTask(i))
	}
	// Task 1
	// Trying to remove the first Task which is in initialized state
	// As initialized task can not be subtracted from allocation so
	// no change in respool allocation
	taskID := fmt.Sprintf("job1-%d", 1)
	t := &peloton.TaskID{Value: taskID}

	rmTask := suite.tracker.GetTask(t)

	resources := &scalar.Resources{
		CPU:    float64(1),
		DISK:   float64(10),
		GPU:    float64(0),
		MEMORY: float64(100),
	}
	rmTask.respool.AddToAllocation(resources)

	res := rmTask.respool.GetAllocation()

	suite.Equal(res, resources)

	deleteTask := &peloton.TaskID{Value: taskID}
	suite.tracker.MarkItDone(deleteTask)

	res = rmTask.respool.GetAllocation()
	suite.Equal(res, resources)

	// FOR TASK 2
	// Trying to remove the Second Task which is in Pending state
	// As pending task can not be subtracted from allocation so
	// no change in respool allocation
	taskID = fmt.Sprintf("job1-%d", 2)
	t = &peloton.TaskID{Value: taskID}

	rmTask = suite.tracker.GetTask(t)

	rmTask.respool.AddToAllocation(resources)

	res = rmTask.respool.GetAllocation()

	err := rmTask.TransitTo(task.TaskState_PENDING.String())
	suite.NoError(err)

	deleteTask = &peloton.TaskID{Value: taskID}
	suite.tracker.MarkItDone(deleteTask)

	res = rmTask.respool.GetAllocation()

	suite.Equal(res, resources)

	// TASK 3
	// Trying to remove the Third Task which is in Ready state
	// As READ task should subtracted from allocation so
	// so respool allocation is zero
	taskID = fmt.Sprintf("job1-%d", 3)
	t = &peloton.TaskID{Value: taskID}
	rmTask = suite.tracker.GetTask(t)
	rmTask.respool.AddToAllocation(resources)

	res = rmTask.respool.GetAllocation()

	err = rmTask.TransitTo(task.TaskState_PENDING.String())
	suite.NoError(err)

	err = rmTask.TransitTo(task.TaskState_READY.String())
	suite.NoError(err)

	deleteTask = &peloton.TaskID{Value: taskID}
	suite.tracker.MarkItDone(deleteTask)

	res = rmTask.respool.GetAllocation()

	zeroResource := &scalar.Resources{
		CPU:    float64(0),
		DISK:   float64(0),
		GPU:    float64(0),
		MEMORY: float64(0),
	}
	suite.Equal(res, zeroResource)

	suite.tracker.Clear()
}
