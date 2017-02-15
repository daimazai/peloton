package task

import (
	"context"
	"fmt"

	"code.uber.internal/infra/peloton/storage"
	log "github.com/Sirupsen/logrus"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/encoding/json"

	"peloton/api/job"
	"peloton/api/task"

	"code.uber.internal/infra/peloton/jobmgr"
)

// InitServiceHandler initializes the TaskManager
func InitServiceHandler(d yarpc.Dispatcher, jobStore storage.JobStore, taskStore storage.TaskStore, metrics *jobmgr.Metrics) {
	handler := serviceHandler{
		taskStore: taskStore,
		jobStore:  jobStore,
		metrics:   metrics,
	}
	json.Register(d, json.Procedure("TaskManager.Get", handler.Get))
	json.Register(d, json.Procedure("TaskManager.List", handler.List))
	json.Register(d, json.Procedure("TaskManager.Start", handler.Start))
	json.Register(d, json.Procedure("TaskManager.Stop", handler.Stop))
	json.Register(d, json.Procedure("TaskManager.Restart", handler.Restart))
}

// serviceHandler implements peloton.api.task.TaskManager
type serviceHandler struct {
	taskStore storage.TaskStore
	jobStore  storage.JobStore
	metrics   *jobmgr.Metrics
}

func (m *serviceHandler) Get(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *task.GetRequest) (*task.GetResponse, yarpc.ResMeta, error) {

	log.Infof("TaskManager.Get called: %v", body)
	m.metrics.TaskAPIGet.Inc(1)
	jobConfig, err := m.jobStore.GetJob(body.JobId)
	if err != nil || jobConfig == nil {
		log.Errorf("Failed to find job with id %v, err=%v", body.JobId, err)
		return &task.GetResponse{
			NotFound: &job.JobNotFound{
				Id:      body.JobId,
				Message: fmt.Sprintf("job %v not found, %v", body.JobId, err),
			},
		}, nil, nil
	}

	result, err := m.taskStore.GetTaskForJob(body.JobId, body.InstanceId)
	for _, taskInfo := range result {
		log.Infof("found task %v", taskInfo)
		m.metrics.TaskGet.Inc(1)
		return &task.GetResponse{
			Result: taskInfo,
		}, nil, nil
	}

	m.metrics.TaskGetFail.Inc(1)
	return &task.GetResponse{
		OutOfRange: &task.InstanceIdOutOfRange{
			JobId:         body.JobId,
			InstanceCount: jobConfig.InstanceCount,
		},
	}, nil, nil
}

func (m *serviceHandler) List(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *task.ListRequest) (*task.ListResponse, yarpc.ResMeta, error) {

	log.Infof("TaskManager.List called: %v", body)
	m.metrics.TaskAPIList.Inc(1)
	_, err := m.jobStore.GetJob(body.JobId)
	if err != nil {
		log.Errorf("Failed to find job with id %v, err=%v", body.JobId, err)
		m.metrics.TaskListFail.Inc(1)
		return &task.ListResponse{
			NotFound: &job.JobNotFound{
				Id:      body.JobId,
				Message: fmt.Sprintf("Failed to find job with id %v, err=%v", body.JobId, err),
			},
		}, nil, nil
	}
	var result map[uint32]*task.TaskInfo
	if body.Range == nil {
		result, err = m.taskStore.GetTasksForJob(body.JobId)
	} else {
		result, err = m.taskStore.GetTasksForJobByRange(body.JobId, body.Range)
	}
	if err != nil || len(result) == 0 {
		m.metrics.TaskListFail.Inc(1)
		return &task.ListResponse{
			NotFound: &job.JobNotFound{
				Id:      body.JobId,
				Message: fmt.Sprintf("err= %v", err),
			},
		}, nil, nil
	}

	m.metrics.TaskList.Inc(1)
	return &task.ListResponse{
		Result: &task.ListResponse_Result{
			Value: result,
		},
	}, nil, nil
}

func (m *serviceHandler) Start(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *task.StartRequest) (*task.StartResponse, yarpc.ResMeta, error) {

	log.Infof("TaskManager.Start called: %v", body)
	m.metrics.TaskAPIStart.Inc(1)
	m.metrics.TaskStart.Inc(1)
	return &task.StartResponse{}, nil, nil
}

func (m *serviceHandler) Stop(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *task.StopRequest) (*task.StopResponse, yarpc.ResMeta, error) {

	log.Infof("TaskManager.Stop called: %v", body)
	m.metrics.TaskAPIStop.Inc(1)
	m.metrics.TaskStop.Inc(1)
	return &task.StopResponse{}, nil, nil
}

func (m *serviceHandler) Restart(
	ctx context.Context,
	reqMeta yarpc.ReqMeta,
	body *task.RestartRequest) (*task.RestartResponse, yarpc.ResMeta, error) {

	log.Infof("TaskManager.Restart called: %v", body)
	m.metrics.TaskAPIRestart.Inc(1)
	m.metrics.TaskRestart.Inc(1)
	return &task.RestartResponse{}, nil, nil
}