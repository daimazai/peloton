package job

import (
	"context"
	"fmt"
	"testing"

	log "github.com/Sirupsen/logrus"
	"github.com/golang/mock/gomock"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"

	"go.uber.org/yarpc"

	mesos "mesos/v1"
	"peloton/api/job"
	"peloton/api/peloton"
	"peloton/api/respool"
	"peloton/api/task"

	"peloton/private/resmgr"
	"peloton/private/resmgrsvc"

	yarpc_mocks "code.uber.internal/infra/peloton/vendor_mocks/go.uber.org/yarpc/encoding/json/mocks"
)

const (
	testInstanceCount = 2
)

var (
	defaultResourceConfig = task.ResourceConfig{
		CpuLimit:    10,
		MemLimitMb:  10,
		DiskLimitMb: 10,
		FdLimit:     10,
	}
)

type JobHandlerTestSuite struct {
	suite.Suite
	handler       *serviceHandler
	testJobID     *peloton.JobID
	testJobConfig *job.JobConfig
	taskInfos     map[uint32]*task.TaskInfo
}

func (suite *JobHandlerTestSuite) SetupTest() {
	mtx := NewMetrics(tally.NoopScope)
	suite.handler = &serviceHandler{
		metrics: mtx,
		rootCtx: context.Background(),
	}
	suite.testJobID = &peloton.JobID{
		Value: "test_job",
	}
	suite.testJobConfig = &job.JobConfig{
		Name:          suite.testJobID.Value,
		InstanceCount: testInstanceCount,
		Sla: &job.SlaConfig{
			Preemptible: true,
			Priority:    22,
		},
	}
	var taskInfos = make(map[uint32]*task.TaskInfo)
	for i := uint32(0); i < testInstanceCount; i++ {
		taskInfos[i] = suite.createTestTaskInfo(
			task.TaskState_RUNNING, i)
	}
	suite.taskInfos = taskInfos
}

func (suite *JobHandlerTestSuite) TearDownTest() {
	log.Debug("tearing down")
}

func TestPelotonJobHanlder(t *testing.T) {
	suite.Run(t, new(JobHandlerTestSuite))
}

func (suite *JobHandlerTestSuite) createTestTaskInfo(
	state task.TaskState,
	instanceID uint32) *task.TaskInfo {

	var taskID = fmt.Sprintf("%s-%d", suite.testJobID.Value, instanceID)
	return &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId:    &mesos.TaskID{Value: &taskID},
			State:     state,
			GoalState: task.TaskState_SUCCEEDED,
		},
		Config: &task.TaskConfig{
			Name:     suite.testJobConfig.Name,
			Resource: &defaultResourceConfig,
		},
		InstanceId: instanceID,
		JobId:      suite.testJobID,
	}
}

func (suite *JobHandlerTestSuite) TestSubmitTasksToResmgr() {
	ctrl := gomock.NewController(suite.T())
	defer ctrl.Finish()

	mockResmgrClient := yarpc_mocks.NewMockClient(ctrl)
	suite.handler.client = mockResmgrClient
	var tasksInfo []*task.TaskInfo
	for _, v := range suite.taskInfos {
		tasksInfo = append(tasksInfo, v)
	}
	tasks := suite.handler.convertToResMgrTask(tasksInfo, suite.testJobConfig)
	var expectedTasks []*resmgr.Task
	gomock.InOrder(
		mockResmgrClient.EXPECT().
			Call(
				gomock.Any(),
				gomock.Eq(yarpc.NewReqMeta().Procedure("ResourceManagerService.EnqueueTasks")),
				gomock.Eq(&resmgrsvc.EnqueueTasksRequest{
					Tasks: tasks,
				}),
				gomock.Any()).
			Do(func(_ context.Context, _ yarpc.CallReqMeta, reqBody interface{}, _ interface{}) {
				req := reqBody.(*resmgrsvc.EnqueueTasksRequest)
				for _, t := range req.Tasks {
					expectedTasks = append(expectedTasks, t)
				}
			}).
			Return(nil, nil),
	)

	suite.handler.enqueueTasks(tasksInfo, suite.testJobConfig)
	suite.Equal(tasks, expectedTasks)
}

func (suite *JobHandlerTestSuite) TestSubmitTasksToResmgrError() {
	ctrl := gomock.NewController(suite.T())
	defer ctrl.Finish()

	mockResmgrClient := yarpc_mocks.NewMockClient(ctrl)
	suite.handler.client = mockResmgrClient
	var tasksInfo []*task.TaskInfo
	for _, v := range suite.taskInfos {
		tasksInfo = append(tasksInfo, v)
	}
	tasks := suite.handler.convertToResMgrTask(tasksInfo, suite.testJobConfig)
	var expectedTasks []*resmgr.Task
	var err error
	gomock.InOrder(
		mockResmgrClient.EXPECT().
			Call(
				gomock.Any(),
				gomock.Eq(yarpc.NewReqMeta().Procedure("ResourceManagerService.EnqueueTasks")),
				gomock.Eq(&resmgrsvc.EnqueueTasksRequest{
					Tasks: tasks,
				}),
				gomock.Any()).
			Do(func(_ context.Context, _ yarpc.CallReqMeta, reqBody interface{}, _ interface{}) {
				req := reqBody.(*resmgrsvc.EnqueueTasksRequest)
				for _, t := range req.Tasks {
					expectedTasks = append(expectedTasks, t)
				}
				err = errors.New("Resmgr Error")
			}).
			Return(nil, err),
	)
	suite.handler.enqueueTasks(tasksInfo, suite.testJobConfig)
	suite.Error(err)
}

func (suite *JobHandlerTestSuite) TestValidateResourcePool() {
	ctrl := gomock.NewController(suite.T())
	defer ctrl.Finish()

	mockResmgrClient := yarpc_mocks.NewMockClient(ctrl)
	suite.handler.client = mockResmgrClient
	respoolID := &respool.ResourcePoolID{
		Value: "respool11",
	}
	var request = &respool.GetRequest{
		Id: respoolID,
	}

	var err error
	gomock.InOrder(
		mockResmgrClient.EXPECT().
			Call(
				gomock.Any(),
				gomock.Eq(yarpc.NewReqMeta().Procedure("ResourceManager.GetResourcePool")),
				gomock.Eq(request),
				gomock.Any()).
			Do(func(_ context.Context, _ yarpc.CallReqMeta, reqBody interface{}, _ interface{}) {
				req := reqBody.(*respool.GetRequest)
				err = errors.New("Respool Not found " + req.Id.Value)
			}).
			Return(nil, err),
	)
	errResponse := suite.handler.validateResourcePool(respoolID)
	suite.Error(errResponse)
}