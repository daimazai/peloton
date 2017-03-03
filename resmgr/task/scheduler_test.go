package task

import (
	"code.uber.internal/infra/peloton/resmgr/queue"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"peloton/api/job"
	pb_respool "peloton/api/respool"
	"peloton/api/task"
	"testing"
	"time"
)

type TaskSchedulerTestSuite struct {
	suite.Suite
	resTree    *respool.Tree
	resPools   map[string]*pb_respool.ResourcePoolConfig
	allNodes   map[string]*respool.ResPool
	root       *respool.ResPool
	readyQueue *queue.MultiLevelList
	taskSched  *Scheduler
}

func (suite *TaskSchedulerTestSuite) SetupTest() {
	suite.resPools = make(map[string]*pb_respool.ResourcePoolConfig)
	suite.allNodes = make(map[string]*respool.ResPool)
	suite.resTree = respool.InitTree(nil, nil, nil)
	suite.setUpRespools()
	suite.root = suite.resTree.CreateTree(nil, respool.RootResPoolID, suite.resPools, suite.allNodes)

	suite.readyQueue = queue.NewMultiLevelList()
	suite.resTree.SetAllNodes(&suite.allNodes)
	suite.AddTasks()
	suite.taskSched = NewTaskScheduler(suite.resTree,
		time.Duration(1)*time.Second,
		suite.readyQueue)
}

func (suite *TaskSchedulerTestSuite) TearDownTest() {
	fmt.Println("tearing down")
}

func TestPelotonTaskScheduler(t *testing.T) {
	suite.Run(t, new(TaskSchedulerTestSuite))
}

func (suite *TaskSchedulerTestSuite) getResourceConfig() []*pb_respool.ResourceConfig {
	resConfigs := make([]*pb_respool.ResourceConfig, 4)
	resConfigcpu := new(pb_respool.ResourceConfig)
	resConfigcpu.Share = 1
	resConfigcpu.Kind = "cpu"
	resConfigcpu.Reservation = 100
	resConfigcpu.Limit = 1000
	resConfigs[0] = resConfigcpu

	resConfigmem := new(pb_respool.ResourceConfig)
	resConfigmem.Share = 1
	resConfigmem.Kind = "memory"
	resConfigmem.Reservation = 100
	resConfigmem.Limit = 1000
	resConfigs[1] = resConfigmem

	resConfigdisk := new(pb_respool.ResourceConfig)
	resConfigdisk.Share = 1
	resConfigdisk.Kind = "disk"
	resConfigdisk.Reservation = 100
	resConfigdisk.Limit = 1000
	resConfigs[2] = resConfigdisk

	resConfiggpu := new(pb_respool.ResourceConfig)
	resConfiggpu.Share = 1
	resConfiggpu.Kind = "gpu"
	resConfiggpu.Reservation = 2
	resConfiggpu.Limit = 4
	resConfigs[3] = resConfiggpu

	return resConfigs
}

func (suite *TaskSchedulerTestSuite) setUpRespools() {
	var parentID pb_respool.ResourcePoolID
	parentID.Value = "root"
	policy := pb_respool.SchedulingPolicy_PriorityFIFO

	var respoolConfigRoot pb_respool.ResourcePoolConfig
	respoolConfigRoot.Name = "root"
	respoolConfigRoot.Parent = nil
	respoolConfigRoot.Resources = suite.getResourceConfig()
	respoolConfigRoot.Policy = policy
	suite.resPools["root"] = &respoolConfigRoot

	var respoolConfig1 pb_respool.ResourcePoolConfig
	respoolConfig1.Name = "respool1"
	respoolConfig1.Parent = &parentID
	respoolConfig1.Resources = suite.getResourceConfig()
	respoolConfig1.Policy = policy
	suite.resPools["respool1"] = &respoolConfig1

	var respoolConfig2 pb_respool.ResourcePoolConfig
	respoolConfig2.Name = "respool2"
	respoolConfig2.Parent = &parentID
	respoolConfig2.Resources = suite.getResourceConfig()
	respoolConfig2.Policy = policy
	suite.resPools["respool2"] = &respoolConfig2

	var respoolConfig3 pb_respool.ResourcePoolConfig
	respoolConfig3.Name = "respool3"
	respoolConfig3.Parent = &parentID
	respoolConfig3.Resources = suite.getResourceConfig()
	respoolConfig3.Policy = policy
	suite.resPools["respool3"] = &respoolConfig3

	var parent1ID pb_respool.ResourcePoolID
	parent1ID.Value = "respool1"

	var respoolConfig11 pb_respool.ResourcePoolConfig
	respoolConfig11.Name = "respool11"
	respoolConfig11.Parent = &parent1ID
	respoolConfig11.Resources = suite.getResourceConfig()
	respoolConfig11.Policy = policy
	suite.resPools["respool11"] = &respoolConfig11

	var respoolConfig12 pb_respool.ResourcePoolConfig
	respoolConfig12.Name = "respool12"
	respoolConfig12.Parent = &parent1ID
	respoolConfig12.Resources = suite.getResourceConfig()
	respoolConfig12.Policy = policy
	suite.resPools["respool12"] = &respoolConfig12

	var parent2ID pb_respool.ResourcePoolID
	parent2ID.Value = "respool2"

	var respoolConfig21 pb_respool.ResourcePoolConfig
	respoolConfig21.Name = "respool21"
	respoolConfig21.Parent = &parent2ID
	respoolConfig21.Resources = suite.getResourceConfig()
	respoolConfig21.Policy = policy
	suite.resPools["respool21"] = &respoolConfig21

	var respoolConfig22 pb_respool.ResourcePoolConfig
	respoolConfig22.Name = "respool22"
	respoolConfig22.Parent = &parent2ID
	respoolConfig22.Resources = suite.getResourceConfig()
	respoolConfig22.Policy = policy
	suite.resPools["respool22"] = &respoolConfig22
}

func (suite *TaskSchedulerTestSuite) AddTasks() {

	// Task -1
	taskInfo1 := task.TaskInfo{
		InstanceId: 1,
		JobId: &job.JobID{
			Value: "job1",
		},
	}
	enq1 := new(queue.TaskItem)
	enq1 = &queue.TaskItem{
		TaskInfo: &taskInfo1,
		TaskID:   fmt.Sprintf("%s-%d", taskInfo1.JobId.Value, taskInfo1.InstanceId),
		Priority: 0,
	}
	suite.allNodes["respool11"].EnqueueTask(enq1)

	// Task -2
	taskInfo2 := task.TaskInfo{
		InstanceId: 2,
		JobId: &job.JobID{
			Value: "job1",
		},
	}

	enq2 := queue.TaskItem{
		TaskInfo: &taskInfo2,
		TaskID:   fmt.Sprintf("%s-%d", taskInfo2.JobId.Value, taskInfo2.InstanceId),
		Priority: 1,
	}
	suite.allNodes["respool11"].EnqueueTask(&enq2)

	// Task -3
	taskInfo3 := task.TaskInfo{
		InstanceId: 1,
		JobId: &job.JobID{
			Value: "job2",
		},
	}

	enq3 := queue.TaskItem{
		TaskInfo: &taskInfo3,
		TaskID:   fmt.Sprintf("%s-%d", taskInfo3.JobId.Value, taskInfo3.InstanceId),
		Priority: 2,
	}
	suite.allNodes["respool11"].EnqueueTask(&enq3)

	// Task -4
	taskInfo4 := task.TaskInfo{
		InstanceId: 2,
		JobId: &job.JobID{
			Value: "job2",
		},
	}

	enq4 := queue.TaskItem{
		TaskInfo: &taskInfo4,
		TaskID:   fmt.Sprintf("%s-%d", taskInfo4.JobId.Value, taskInfo4.InstanceId),
		Priority: 2,
	}
	suite.allNodes["respool11"].EnqueueTask(&enq4)
}

func (suite *TaskSchedulerTestSuite) TestMovingtoReadyQueue() {
	// TODO: Test start and stop differently
	suite.taskSched.Start()
	time.Sleep(10000 * time.Millisecond)
	assert.Equal(suite.T(), suite.readyQueue.Len(2), 2, "Length should be 2 at priority 2")
	assert.Equal(suite.T(), suite.readyQueue.Len(0), 1, "Length should be 1 at priority 0")
	assert.Equal(suite.T(), suite.readyQueue.Len(1), 1, "Length should be 1 at priority 1")
}

func (suite *TaskSchedulerTestSuite) TestMovingTasks() {
	suite.taskSched.scheduleTasks()
	assert.Equal(suite.T(), suite.readyQueue.Len(2), 2, "Length should be 2 at priority 2")
	assert.Equal(suite.T(), suite.readyQueue.Len(0), 1, "Length should be 1 at priority 0")
	assert.Equal(suite.T(), suite.readyQueue.Len(1), 1, "Length should be 1 at priority 1")
}