package event

import (
	"context"
	"fmt"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	pb_eventstream "code.uber.internal/infra/peloton/.gen/peloton/private/eventstream"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"

	res_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"
	jm_task "code.uber.internal/infra/peloton/jobmgr/task"
	store_mocks "code.uber.internal/infra/peloton/storage/mocks"
	"code.uber.internal/infra/peloton/util"
)

const (
	_waitTime = 1 * time.Second
)

type TaskUpdaterTestSuite struct {
	suite.Suite

	updater          *statusUpdate
	ctrl             *gomock.Controller
	testScope        tally.TestScope
	mockResmgrClient *res_mocks.MockResourceManagerServiceYarpcClient
	mockJobStore     *store_mocks.MockJobStore
	mockTaskStore    *store_mocks.MockTaskStore
}

func (suite *TaskUpdaterTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.testScope = tally.NewTestScope("", map[string]string{})
	suite.mockResmgrClient = res_mocks.NewMockResourceManagerServiceYarpcClient(suite.ctrl)
	suite.mockJobStore = store_mocks.NewMockJobStore(suite.ctrl)
	suite.mockTaskStore = store_mocks.NewMockTaskStore(suite.ctrl)
	suite.testScope = tally.NewTestScope("", map[string]string{})

	suite.updater = &statusUpdate{
		jobStore:     suite.mockJobStore,
		taskStore:    suite.mockTaskStore,
		rootCtx:      context.Background(),
		resmgrClient: suite.mockResmgrClient,
		metrics:      NewMetrics(suite.testScope),
	}
}

func (suite *TaskUpdaterTestSuite) TearDownTest() {
	log.Debug("tearing down")
}

func TestPelotonTaskUpdater(t *testing.T) {
	suite.Run(t, new(TaskUpdaterTestSuite))
}

// Test happy case of processing status update.
func (suite *TaskUpdaterTestSuite) TestProcessStatusUpdate() {
	defer suite.ctrl.Finish()

	jobID := uuid.NewUUID().String()
	uuidStr := uuid.NewUUID().String()
	instanceID := 0
	mesosTaskID := fmt.Sprintf("%s-%d-%s", jobID, instanceID, uuidStr)
	pelotonTaskID := fmt.Sprintf("%s-%d", jobID, instanceID)
	state := mesos.TaskState_TASK_RUNNING
	taskStatus := &mesos.TaskStatus{
		TaskId: &mesos.TaskID{
			Value: &mesosTaskID,
		},
		State: &state,
	}
	event := &pb_eventstream.Event{
		MesosTaskStatus: taskStatus,
		Type:            pb_eventstream.Event_MESOS_TASK_STATUS,
	}
	taskInfo := &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId:    &mesos.TaskID{Value: &mesosTaskID},
			State:     task.TaskState_INITIALIZED,
			GoalState: task.TaskState_SUCCEEDED,
		},
	}
	updateTaskInfo := &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId:    &mesos.TaskID{Value: &mesosTaskID},
			State:     task.TaskState_RUNNING,
			GoalState: task.TaskState_SUCCEEDED,
		},
	}

	gomock.InOrder(
		suite.mockTaskStore.EXPECT().
			GetTaskByID(context.Background(), pelotonTaskID).
			Return(taskInfo, nil),
		suite.mockTaskStore.EXPECT().
			UpdateTask(context.Background(), updateTaskInfo).
			Return(nil),
	)
	suite.NoError(suite.updater.ProcessStatusUpdate(context.Background(), event))
}

// Test processing task failure status update w/ retry.
func (suite *TaskUpdaterTestSuite) TestProcessTaskFailedStatusUpdateWithRetry() {
	defer suite.ctrl.Finish()

	jobID := uuid.NewUUID().String()
	uuidStr := uuid.NewUUID().String()
	instanceID := 0
	mesosTaskID := fmt.Sprintf("%s-%d-%s", jobID, instanceID, uuidStr)
	pelotonTaskID := fmt.Sprintf("%s-%d", jobID, instanceID)
	state := mesos.TaskState_TASK_FAILED
	mesosReason := mesos.TaskStatus_REASON_CONTAINER_LAUNCH_FAILED
	failureMsg := "testFailure"
	taskStatus := &mesos.TaskStatus{
		TaskId: &mesos.TaskID{
			Value: &mesosTaskID,
		},
		State:   &state,
		Reason:  &mesosReason,
		Message: &failureMsg,
	}
	event := &pb_eventstream.Event{
		MesosTaskStatus: taskStatus,
		Type:            pb_eventstream.Event_MESOS_TASK_STATUS,
	}
	sla := &job.SlaConfig{
		Preemptible: false,
	}
	jobConfig := &job.JobConfig{
		Name:          jobID,
		Sla:           sla,
		InstanceCount: 1,
	}
	pelotonJobID := &peloton.JobID{
		Value: jobID,
	}
	taskInfo := &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId:    &mesos.TaskID{Value: &mesosTaskID},
			State:     task.TaskState_RUNNING,
			GoalState: task.TaskState_SUCCEEDED,
		},
		Config: &task.TaskConfig{
			Name: jobID,
			RestartPolicy: &task.RestartPolicy{
				MaxFailures: 3,
			},
		},
		InstanceId: uint32(instanceID),
		JobId: &peloton.JobID{
			Value: jobID,
		},
	}

	tasks := []*task.TaskInfo{taskInfo}
	gangs := jm_task.ConvertToResMgrGangs(tasks, jobConfig)
	rescheduleMsg := "Rescheduled due to task failure status: testFailure"
	suite.mockTaskStore.EXPECT().
		GetTaskByID(context.Background(), pelotonTaskID).
		Return(taskInfo, nil)
	suite.mockJobStore.EXPECT().
		GetJobConfig(context.Background(), pelotonJobID).
		Return(jobConfig, nil)
	suite.mockResmgrClient.EXPECT().
		EnqueueGangs(
			gomock.Any(),
			gomock.Eq(&resmgrsvc.EnqueueGangsRequest{
				Gangs: gangs,
			})).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil)
	suite.mockTaskStore.EXPECT().
		UpdateTask(context.Background(), gomock.Any()).
		Do(func(ctx context.Context, updateTask *task.TaskInfo) {
			suite.Equal(updateTask.JobId, pelotonJobID)
			suite.Equal(
				updateTask.Runtime.State,
				task.TaskState_INITIALIZED,
			)
			suite.Equal(
				updateTask.Runtime.Reason,
				mesosReason.String(),
			)
			suite.Equal(
				updateTask.Runtime.Message,
				rescheduleMsg,
			)
			suite.Equal(
				updateTask.Runtime.FailuresCount,
				uint32(1),
			)
		}).
		Return(nil)
	suite.NoError(suite.updater.ProcessStatusUpdate(context.Background(), event))
	time.Sleep(_waitTime)
}

// Test processing task failure status update w/o retry.
func (suite *TaskUpdaterTestSuite) TestProcessTaskFailedStatusUpdateNoRetry() {
	defer suite.ctrl.Finish()

	jobID := uuid.NewUUID().String()
	uuidStr := uuid.NewUUID().String()
	instanceID := 0
	mesosTaskID := fmt.Sprintf("%s-%d-%s", jobID, instanceID, uuidStr)
	pelotonTaskID := fmt.Sprintf("%s-%d", jobID, instanceID)
	state := mesos.TaskState_TASK_FAILED
	mesosReason := mesos.TaskStatus_REASON_CONTAINER_LAUNCH_FAILED
	taskStatus := &mesos.TaskStatus{
		TaskId: &mesos.TaskID{
			Value: &mesosTaskID,
		},
		State:  &state,
		Reason: &mesosReason,
	}
	event := &pb_eventstream.Event{
		MesosTaskStatus: taskStatus,
		Type:            pb_eventstream.Event_MESOS_TASK_STATUS,
	}

	taskInfo := &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId:    &mesos.TaskID{Value: &mesosTaskID},
			State:     task.TaskState_INITIALIZED,
			GoalState: task.TaskState_SUCCEEDED,
		},
	}
	updateTaskInfo := &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId:    &mesos.TaskID{Value: &mesosTaskID},
			State:     task.TaskState_FAILED,
			GoalState: task.TaskState_SUCCEEDED,
			Reason:    mesosReason.String(),
		},
	}

	gomock.InOrder(
		suite.mockTaskStore.EXPECT().
			GetTaskByID(context.Background(), pelotonTaskID).
			Return(taskInfo, nil),
		suite.mockTaskStore.EXPECT().
			UpdateTask(context.Background(), updateTaskInfo).
			Return(nil),
	)
	suite.NoError(suite.updater.ProcessStatusUpdate(context.Background(), event))
}

// Test processing task failure status update w/ retry.
func (suite *TaskUpdaterTestSuite) TestProcessTaskFailedRetryDBFailure() {
	defer suite.ctrl.Finish()

	jobID := uuid.NewUUID().String()
	uuidStr := uuid.NewUUID().String()
	instanceID := 0
	mesosTaskID := fmt.Sprintf("%s-%d-%s", jobID, instanceID, uuidStr)
	pelotonTaskID := fmt.Sprintf("%s-%d", jobID, instanceID)
	state := mesos.TaskState_TASK_FAILED
	mesosReason := mesos.TaskStatus_REASON_CONTAINER_LAUNCH_FAILED
	failureMsg := "testFailure"
	taskStatus := &mesos.TaskStatus{
		TaskId: &mesos.TaskID{
			Value: &mesosTaskID,
		},
		State:   &state,
		Reason:  &mesosReason,
		Message: &failureMsg,
	}
	event := &pb_eventstream.Event{
		MesosTaskStatus: taskStatus,
		Type:            pb_eventstream.Event_MESOS_TASK_STATUS,
	}
	sla := &job.SlaConfig{
		Preemptible: false,
	}
	jobConfig := &job.JobConfig{
		Name:          jobID,
		Sla:           sla,
		InstanceCount: 1,
	}
	pelotonJobID := &peloton.JobID{
		Value: jobID,
	}
	taskInfo := &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId:    &mesos.TaskID{Value: &mesosTaskID},
			State:     task.TaskState_RUNNING,
			GoalState: task.TaskState_SUCCEEDED,
		},
		Config: &task.TaskConfig{
			Name: jobID,
			RestartPolicy: &task.RestartPolicy{
				MaxFailures: 3,
			},
		},
		InstanceId: uint32(instanceID),
		JobId: &peloton.JobID{
			Value: jobID,
		},
	}

	var resmgrTasks []*resmgr.Task
	resmgrTasks = append(
		resmgrTasks,
		util.ConvertTaskToResMgrTask(taskInfo, jobConfig),
	)
	rescheduleMsg := "Rescheduled due to task failure status: testFailure"

	suite.mockTaskStore.EXPECT().
		GetTaskByID(context.Background(), pelotonTaskID).
		Return(taskInfo, nil)
	suite.mockJobStore.EXPECT().
		GetJobConfig(context.Background(), pelotonJobID).
		Return(jobConfig, errors.New("testError"))
	suite.mockTaskStore.EXPECT().
		UpdateTask(context.Background(), gomock.Any()).
		Do(func(ctx context.Context, updateTask *task.TaskInfo) {
			suite.Equal(updateTask.JobId, pelotonJobID)
			suite.Equal(
				updateTask.Runtime.State,
				task.TaskState_INITIALIZED,
			)
			suite.Equal(
				updateTask.Runtime.Reason,
				mesosReason.String(),
			)
			suite.Equal(
				updateTask.Runtime.Message,
				rescheduleMsg,
			)
			suite.Equal(
				updateTask.Runtime.FailuresCount,
				uint32(1),
			)
		}).
		Return(nil)
	suite.NoError(suite.updater.ProcessStatusUpdate(context.Background(), event))
	time.Sleep(_waitTime)
}

func (suite *TaskUpdaterTestSuite) TestIsErrorState() {
	suite.True(isUnexpected(task.TaskState_FAILED))
	suite.True(isUnexpected(task.TaskState_LOST))

	suite.False(isUnexpected(task.TaskState_KILLED))
	suite.False(isUnexpected(task.TaskState_LAUNCHING))
	suite.False(isUnexpected(task.TaskState_RUNNING))
	suite.False(isUnexpected(task.TaskState_SUCCEEDED))
	suite.False(isUnexpected(task.TaskState_INITIALIZED))
}