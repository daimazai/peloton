package stateless

import (
	"context"
	"encoding/base64"
	"sort"
	"time"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	pbjob "code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/respool"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v1alpha/job/stateless"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v1alpha/job/stateless/svc"
	v1alphapeloton "code.uber.internal/infra/peloton/.gen/peloton/api/v1alpha/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v1alpha/pod"
	pelotonv1alphaquery "code.uber.internal/infra/peloton/.gen/peloton/api/v1alpha/query"
	"code.uber.internal/infra/peloton/.gen/peloton/private/models"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/jobmgr/cached"
	jobmgrcommon "code.uber.internal/infra/peloton/jobmgr/common"
	"code.uber.internal/infra/peloton/jobmgr/goalstate"
	"code.uber.internal/infra/peloton/jobmgr/job/config"
	"code.uber.internal/infra/peloton/jobmgr/jobsvc"
	jobmgrtask "code.uber.internal/infra/peloton/jobmgr/task"
	handlerutil "code.uber.internal/infra/peloton/jobmgr/util/handler"
	jobutil "code.uber.internal/infra/peloton/jobmgr/util/job"
	"code.uber.internal/infra/peloton/leader"
	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/util"

	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/yarpcerrors"
)

type serviceHandler struct {
	jobStore        storage.JobStore
	updateStore     storage.UpdateStore
	secretStore     storage.SecretStore
	taskStore       storage.TaskStore
	respoolClient   respool.ResourceManagerYARPCClient
	jobFactory      cached.JobFactory
	goalStateDriver goalstate.Driver
	candidate       leader.Candidate
	rootCtx         context.Context
	jobSvcCfg       jobsvc.Config
}

var (
	errNullResourcePoolID   = yarpcerrors.InvalidArgumentErrorf("resource pool ID is null")
	errResourcePoolNotFound = yarpcerrors.NotFoundErrorf("resource pool not found")
	errRootResourcePoolID   = yarpcerrors.InvalidArgumentErrorf("cannot submit jobs to the `root` resource pool")
	errNonLeafResourcePool  = yarpcerrors.InvalidArgumentErrorf("cannot submit jobs to a non leaf resource pool")
)

// InitV1AlphaJobServiceHandler initializes the Job Manager V1Alpha Service Handler
func InitV1AlphaJobServiceHandler(
	d *yarpc.Dispatcher,
	jobStore storage.JobStore,
	updateStore storage.UpdateStore,
	secretStore storage.SecretStore,
	taskStore storage.TaskStore,
	jobFactory cached.JobFactory,
	goalStateDriver goalstate.Driver,
	candidate leader.Candidate,
	jobSvcCfg jobsvc.Config,
) {
	handler := &serviceHandler{
		jobStore:        jobStore,
		updateStore:     updateStore,
		secretStore:     secretStore,
		taskStore:       taskStore,
		respoolClient:   respool.NewResourceManagerYARPCClient(d.ClientConfig(common.PelotonResourceManager)),
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
		candidate:       candidate,
		jobSvcCfg:       jobSvcCfg,
	}
	d.Register(svc.BuildJobServiceYARPCProcedures(handler))
}

func (h *serviceHandler) CreateJob(
	ctx context.Context,
	req *svc.CreateJobRequest,
) (resp *svc.CreateJobResponse, err error) {
	defer func() {
		jobID := req.GetJobId().GetValue()
		specVersion := req.GetSpec().GetRevision().GetVersion()
		instanceCount := req.GetSpec().GetInstanceCount()

		if err != nil {
			log.WithField("job_id", jobID).
				WithField("spec_version", specVersion).
				WithField("instace_count", instanceCount).
				WithError(err).
				Warn("JobSVC.CreateJob failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("job_id", jobID).
			WithField("spec_version", specVersion).
			WithField("response", resp).
			WithField("instace_count", instanceCount).
			Info("JobSVC.CreateJob succeeded")
	}()

	if !h.candidate.IsLeader() {
		return nil,
			yarpcerrors.UnavailableErrorf("JobSVC.CreateJob is not supported on non-leader")
	}

	pelotonJobID := &peloton.JobID{Value: req.GetJobId().GetValue()}

	// It is possible that jobId is nil since protobuf doesn't enforce it
	if len(pelotonJobID.GetValue()) == 0 {
		pelotonJobID = &peloton.JobID{Value: uuid.New()}
	}

	if uuid.Parse(pelotonJobID.GetValue()) == nil {
		return nil, yarpcerrors.InvalidArgumentErrorf("jobID is not valid UUID")
	}

	jobSpec := req.GetSpec()

	respoolPath, err := h.validateResourcePoolForJobCreation(ctx, jobSpec.GetRespoolId())
	if err != nil {
		return nil, errors.Wrap(err, "failed to validate resource pool")
	}

	jobConfig, err := handlerutil.ConvertJobSpecToJobConfig(jobSpec)
	if err != nil {
		return nil, err
	}

	// Validate job config with default task configs
	err = jobconfig.ValidateConfig(
		jobConfig,
		h.jobSvcCfg.MaxTasksPerJob,
	)
	if err != nil {
		return nil, errors.Wrap(err, "invalid JobSpec")
	}

	// check secrets and config for input sanity
	if err = h.validateSecretsAndConfig(jobSpec, req.GetSecrets()); err != nil {
		return nil, errors.Wrap(err, "input cannot contain secret volume")
	}

	// create secrets in the DB and add them as secret volumes to defaultconfig
	err = h.handleCreateSecrets(ctx, pelotonJobID.GetValue(), jobSpec, req.GetSecrets())
	if err != nil {
		return nil, errors.Wrap(err, "failed to handle create-secrets")
	}

	// Create job in cache and db
	cachedJob := h.jobFactory.AddJob(pelotonJobID)

	systemLabels := jobutil.ConstructSystemLabels(jobConfig, respoolPath.GetValue())
	configAddOn := &models.ConfigAddOn{
		SystemLabels: systemLabels,
	}
	err = cachedJob.Create(ctx, jobConfig, configAddOn, "peloton")

	// enqueue the job into goal state engine even in failure case.
	// Because the state may be updated, let goal state engine decide what to do
	h.goalStateDriver.EnqueueJob(pelotonJobID, time.Now())

	if err != nil {
		return nil, err
	}

	runtimeInfo, err := cachedJob.GetRuntime(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get job runtime from cache")
	}

	return &svc.CreateJobResponse{
		JobId: &v1alphapeloton.JobID{Value: pelotonJobID.GetValue()},
		Version: jobutil.GetJobEntityVersion(
			uint64(runtimeInfo.GetConfigurationVersion()),
			runtimeInfo.GetWorkflowVersion(),
		),
	}, nil
}

func (h *serviceHandler) ReplaceJob(
	ctx context.Context,
	req *svc.ReplaceJobRequest) (resp *svc.ReplaceJobResponse, err error) {
	defer func() {
		jobID := req.GetJobId().GetValue()
		specVersion := req.GetSpec().GetRevision().GetVersion()
		entityVersion := req.GetVersion().GetValue()

		if err != nil {
			log.WithField("job_id", jobID).
				WithField("spec_version", specVersion).
				WithField("entity_version", entityVersion).
				WithError(err).
				Warn("JobSVC.ReplaceJob failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("job_id", jobID).
			WithField("spec_version", specVersion).
			WithField("entity_version", entityVersion).
			WithField("response", resp).
			Info("JobSVC.ReplaceJob succeeded")
	}()

	// TODO: handle secretes
	jobUUID := uuid.Parse(req.GetJobId().GetValue())
	if jobUUID == nil {
		return nil, yarpcerrors.InvalidArgumentErrorf(
			"JobID must be of UUID format")
	}

	jobID := &peloton.JobID{Value: req.GetJobId().GetValue()}

	cachedJob := h.jobFactory.AddJob(jobID)
	jobRuntime, err := cachedJob.GetRuntime(ctx)
	if err != nil {
		return nil, err
	}

	// do not allow update initialized job for now, because
	// the job would be updated by both job and update goal
	// state engine. The constraint may be removed later
	if jobRuntime.GetState() == pbjob.JobState_INITIALIZED {
		return nil, yarpcerrors.UnavailableErrorf(
			"cannot update partially created job")
	}

	jobConfig, err := handlerutil.ConvertJobSpecToJobConfig(req.GetSpec())
	if err != nil {
		return nil, err
	}
	prevJobConfig, prevConfigAddOn, err := h.jobStore.GetJobConfigWithVersion(
		ctx,
		jobID,
		jobRuntime.GetConfigurationVersion())
	if err != nil {
		return nil, err
	}

	if err := validateJobConfigUpdate(prevJobConfig, jobConfig); err != nil {
		return nil, err
	}

	// get the new configAddOn
	var respoolPath string
	for _, label := range prevConfigAddOn.GetSystemLabels() {
		if label.GetKey() == common.SystemLabelResourcePool {
			respoolPath = label.GetValue()
		}
	}
	configAddOn := &models.ConfigAddOn{
		SystemLabels: jobutil.ConstructSystemLabels(jobConfig, respoolPath),
	}

	opaque := cached.WithOpaqueData(nil)
	if req.GetOpaqueData() != nil {
		opaque = cached.WithOpaqueData(&peloton.OpaqueData{
			Data: req.GetOpaqueData().GetData(),
		})
	}

	// if change log is set, CreateWorkflow would use the version inside
	// to do concurrency control.
	// However, for replace job, concurrency control is done by entity version.
	// User should not be required to provide config version when entity version is
	// provided.
	jobConfig.ChangeLog = nil
	updateID, newEntityVersion, err := cachedJob.CreateWorkflow(
		ctx,
		models.WorkflowType_UPDATE,
		handlerutil.ConvertUpdateSpecToUpdateConfig(req.GetUpdateSpec()),
		req.GetVersion(),
		cached.WithConfig(jobConfig, prevJobConfig, configAddOn),
		opaque,
	)

	// In case of error, since it is not clear if job runtime was
	// persisted with the update ID or not, enqueue the update to
	// the goal state. If the update ID got persisted, update should
	// start running, else, it should be aborted. Enqueueing it into
	// the goal state will ensure both. In case the update was not
	// persisted, clear the cache as well so that it is reloaded
	// from DB and cleaned up.
	if len(updateID.GetValue()) > 0 {
		h.goalStateDriver.EnqueueUpdate(jobID, updateID, time.Now())
	}

	if err != nil {
		return nil, err
	}

	return &svc.ReplaceJobResponse{Version: newEntityVersion}, nil
}

func (h *serviceHandler) PatchJob(
	ctx context.Context,
	req *svc.PatchJobRequest) (*svc.PatchJobResponse, error) {
	return &svc.PatchJobResponse{}, nil
}

func (h *serviceHandler) RestartJob(
	ctx context.Context,
	req *svc.RestartJobRequest) (*svc.RestartJobResponse, error) {
	return &svc.RestartJobResponse{}, nil
}

func (h *serviceHandler) PauseJobWorkflow(
	ctx context.Context,
	req *svc.PauseJobWorkflowRequest) (resp *svc.PauseJobWorkflowResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Warn("JobSVC.PauseJobWorkflow failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("request", req).
			WithField("response", resp).
			Info("JobSVC.PauseJobWorkflow succeeded")
	}()

	cachedJob := h.jobFactory.AddJob(&peloton.JobID{Value: req.GetJobId().GetValue()})
	opaque := cached.WithOpaqueData(nil)
	if req.GetOpaqueData() != nil {
		opaque = cached.WithOpaqueData(&peloton.OpaqueData{
			Data: req.GetOpaqueData().GetData(),
		})
	}

	updateID, newEntityVersion, err := cachedJob.PauseWorkflow(
		ctx,
		req.GetVersion(),
		opaque,
	)

	if len(updateID.GetValue()) > 0 {
		h.goalStateDriver.EnqueueUpdate(cachedJob.ID(), updateID, time.Now())
	}

	if err != nil {
		return nil, err
	}

	return &svc.PauseJobWorkflowResponse{Version: newEntityVersion}, nil
}

func (h *serviceHandler) ResumeJobWorkflow(
	ctx context.Context,
	req *svc.ResumeJobWorkflowRequest) (resp *svc.ResumeJobWorkflowResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Warn("JobSVC.ResumeJobWorkflow failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("request", req).
			WithField("response", resp).
			Info("JobSVC.ResumeJobWorkflow succeeded")
	}()

	cachedJob := h.jobFactory.AddJob(&peloton.JobID{Value: req.GetJobId().GetValue()})
	opaque := cached.WithOpaqueData(nil)
	if req.GetOpaqueData() != nil {
		opaque = cached.WithOpaqueData(&peloton.OpaqueData{
			Data: req.GetOpaqueData().GetData(),
		})
	}

	updateID, newEntityVersion, err := cachedJob.ResumeWorkflow(
		ctx,
		req.GetVersion(),
		opaque,
	)

	if len(updateID.GetValue()) > 0 {
		h.goalStateDriver.EnqueueUpdate(cachedJob.ID(), updateID, time.Now())
	}

	if err != nil {
		return nil, err
	}

	return &svc.ResumeJobWorkflowResponse{Version: newEntityVersion}, nil
}

func (h *serviceHandler) AbortJobWorkflow(
	ctx context.Context,
	req *svc.AbortJobWorkflowRequest) (resp *svc.AbortJobWorkflowResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Warn("JobSVC.AbortJobWorkflow failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("request", req).
			WithField("response", resp).
			Info("JobSVC.AbortJobWorkflow succeeded")
	}()

	cachedJob := h.jobFactory.AddJob(&peloton.JobID{Value: req.GetJobId().GetValue()})
	opaque := cached.WithOpaqueData(nil)
	if req.GetOpaqueData() != nil {
		opaque = cached.WithOpaqueData(&peloton.OpaqueData{
			Data: req.GetOpaqueData().GetData(),
		})
	}

	updateID, newEntityVersion, err := cachedJob.AbortWorkflow(
		ctx,
		req.GetVersion(),
		opaque,
	)

	if len(updateID.GetValue()) > 0 {
		h.goalStateDriver.EnqueueUpdate(cachedJob.ID(), updateID, time.Now())
	}

	if err != nil {
		return nil, err
	}

	return &svc.AbortJobWorkflowResponse{Version: newEntityVersion}, nil
}

func (h *serviceHandler) StartJob(
	ctx context.Context,
	req *svc.StartJobRequest) (*svc.StartJobResponse, error) {
	return &svc.StartJobResponse{}, nil
}
func (h *serviceHandler) StopJob(
	ctx context.Context,
	req *svc.StopJobRequest) (*svc.StopJobResponse, error) {
	return &svc.StopJobResponse{}, nil
}
func (h *serviceHandler) DeleteJob(
	ctx context.Context,
	req *svc.DeleteJobRequest) (*svc.DeleteJobResponse, error) {
	return &svc.DeleteJobResponse{}, nil
}

func (h *serviceHandler) getJobSummary(
	ctx context.Context,
	jobID *v1alphapeloton.JobID) (*svc.GetJobResponse, error) {
	jobSummary, err := h.jobStore.GetJobSummaryFromIndex(
		ctx,
		&peloton.JobID{Value: jobID.GetValue()},
	)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get job summary")
	}

	var updateInfo *models.UpdateModel
	if len(jobSummary.GetRuntime().GetUpdateID().GetValue()) > 0 {
		updateInfo, err = h.updateStore.GetUpdate(
			ctx,
			jobSummary.GetRuntime().GetUpdateID(),
		)
		if err != nil {
			return nil, errors.Wrap(err, "fail to get update information")
		}
	}

	return &svc.GetJobResponse{
		Summary: handlerutil.ConvertJobSummary(jobSummary, updateInfo),
	}, nil
}

func (h *serviceHandler) getJobConfigurationWithVersion(
	ctx context.Context,
	jobID *v1alphapeloton.JobID,
	version *v1alphapeloton.EntityVersion) (*svc.GetJobResponse, error) {
	configVersion, _, err := jobutil.ParseJobEntityVersion(version)
	if err != nil {
		return nil, err
	}

	jobConfig, _, err := h.jobStore.GetJobConfigWithVersion(
		ctx,
		&peloton.JobID{Value: jobID.GetValue()},
		configVersion,
	)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get job spec")
	}

	return &svc.GetJobResponse{
		JobInfo: &stateless.JobInfo{
			JobId: jobID,
			Spec:  handlerutil.ConvertJobConfigToJobSpec(jobConfig),
		},
	}, nil
}

func (h *serviceHandler) GetJob(
	ctx context.Context,
	req *svc.GetJobRequest) (resp *svc.GetJobResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Info("StatelessJobSvc.GetJob failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("req", req).
			Debug("StatelessJobSvc.GetJob succeeded")
	}()

	// Get the summary only
	if req.GetSummaryOnly() == true {
		return h.getJobSummary(ctx, req.GetJobId())
	}

	// Get the configuration for a given version only
	if req.GetVersion() != nil {
		return h.getJobConfigurationWithVersion(ctx, req.GetJobId(), req.GetVersion())
	}

	// Get the latest configuration and runtime
	jobConfig, _, err := h.jobStore.GetJobConfig(
		ctx,
		&peloton.JobID{Value: req.GetJobId().GetValue()},
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get job spec")
	}

	// Do not display the secret volumes in defaultconfig that were added by
	// handleSecrets. They should remain internal to peloton logic.
	// Secret ID and Path should be returned using the peloton.Secret
	// proto message.
	secretVolumes := util.RemoveSecretVolumesFromJobConfig(jobConfig)

	jobRuntime, err := h.jobStore.GetJobRuntime(
		ctx,
		&peloton.JobID{Value: req.GetJobId().GetValue()},
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get job status")
	}

	var updateInfo *models.UpdateModel
	if len(jobRuntime.GetUpdateID().GetValue()) > 0 {
		updateInfo, err = h.updateStore.GetUpdate(
			ctx,
			jobRuntime.GetUpdateID(),
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get update information")
		}
	}

	return &svc.GetJobResponse{
		JobInfo: &stateless.JobInfo{
			JobId:  req.GetJobId(),
			Spec:   handlerutil.ConvertJobConfigToJobSpec(jobConfig),
			Status: handlerutil.ConvertRuntimeInfoToJobStatus(jobRuntime, updateInfo),
		},
		Secrets: handlerutil.ConvertV0SecretsToV1Secrets(
			jobmgrtask.CreateSecretsFromVolumes(secretVolumes)),
		WorkflowInfo: handlerutil.ConvertUpdateModelToWorkflowInfo(updateInfo),
	}, nil
}

// GetJobIDFromJobName looks up job ids for provided job name and
// job ids are returned in descending create timestamp
func (h *serviceHandler) GetJobIDFromJobName(
	ctx context.Context,
	req *svc.GetJobIDFromJobNameRequest) (resp *svc.GetJobIDFromJobNameResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Info("StatelessJobSvc.GetJobIDFromJobName failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("req", req).
			Debug("StatelessJobSvc.GetJobIDFromJobName succeeded")
	}()

	jobID, err := h.jobStore.GetJobIDFromJobName(ctx, req.GetJobName())
	if err != nil {
		return nil, errors.Wrap(err, "failed to get job identifiers from job name")
	}

	return &svc.GetJobIDFromJobNameResponse{
		JobId: jobID,
	}, nil
}

func (h *serviceHandler) GetWorkflowEvents(
	ctx context.Context,
	req *svc.GetWorkflowEventsRequest) (*svc.GetWorkflowEventsResponse, error) {
	return &svc.GetWorkflowEventsResponse{}, nil
}
func (h *serviceHandler) ListPods(
	req *svc.ListPodsRequest,
	stream svc.JobServiceServiceListPodsYARPCServer) error {
	return nil
}
func (h *serviceHandler) QueryPods(
	ctx context.Context,
	req *svc.QueryPodsRequest) (*svc.QueryPodsResponse, error) {
	return &svc.QueryPodsResponse{}, nil
}

func (h *serviceHandler) QueryJobs(
	ctx context.Context,
	req *svc.QueryJobsRequest) (resp *svc.QueryJobsResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Warn("JobSVC.QueryJobs failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("request", req).
			WithField("num_of_results", len(resp.GetRecords())).
			Debug("JobSVC.QueryJobs succeeded")
	}()

	var respoolID *peloton.ResourcePoolID
	if len(req.GetSpec().GetRespool().GetValue()) > 0 {
		respoolResp, err := h.respoolClient.LookupResourcePoolID(ctx, &respool.LookupRequest{
			Path: &respool.ResourcePoolPath{Value: req.GetSpec().GetRespool().GetValue()},
		})
		if err != nil {
			return nil, errors.Wrap(err, "fail to get respool id")
		}
		respoolID = respoolResp.GetId()
	}

	querySpec := handlerutil.ConvertStatelessQuerySpecToJobQuerySpec(req.GetSpec())
	log.WithField("spec", querySpec).
		Info("converted spec")
	_, jobSummaries, total, err := h.jobStore.QueryJobs(
		ctx,
		respoolID,
		querySpec,
		true)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get job summary")
	}

	var statelessJobSummaries []*stateless.JobSummary
	for _, jobSummary := range jobSummaries {
		var statelessJobLabels []*v1alphapeloton.Label
		for _, label := range jobSummary.GetLabels() {
			statelessJobLabels = append(statelessJobLabels, &v1alphapeloton.Label{
				Key:   label.GetKey(),
				Value: label.GetValue(),
			})
		}

		var updateModel *models.UpdateModel
		if len(jobSummary.GetRuntime().GetUpdateID().GetValue()) > 0 {
			updateModel, err = h.updateStore.GetUpdate(ctx, jobSummary.GetRuntime().GetUpdateID())
			if err != nil {
				return nil, errors.Wrap(err, "fail to get update")
			}
		}

		statelessJobSummary := handlerutil.ConvertJobSummary(jobSummary, updateModel)
		statelessJobSummaries = append(statelessJobSummaries, statelessJobSummary)
	}

	return &svc.QueryJobsResponse{
		Records: statelessJobSummaries,
		Pagination: &pelotonv1alphaquery.Pagination{
			Offset: req.GetSpec().GetPagination().GetOffset(),
			Limit:  req.GetSpec().GetPagination().GetLimit(),
			Total:  total,
		},
		Spec: req.GetSpec(),
	}, nil
}

func (h *serviceHandler) ListJobs(
	req *svc.ListJobsRequest,
	stream svc.JobServiceServiceListJobsYARPCServer) (err error) {
	defer func() {
		if err != nil {
			log.WithError(err).
				Info("JobSVC.ListJobs failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.Debug("JobSVC.ListJobs succeeded")
	}()

	jobSummaries, err := h.jobStore.GetAllJobsInJobIndex(context.Background())
	if err != nil {
		return err
	}

	for _, jobSummary := range jobSummaries {
		var updateInfo *models.UpdateModel

		if len(jobSummary.GetRuntime().GetUpdateID().GetValue()) > 0 {
			updateInfo, err = h.updateStore.GetUpdate(
				context.Background(),
				jobSummary.GetRuntime().GetUpdateID(),
			)
			if err != nil {
				return err
			}
		}

		resp := &svc.ListJobsResponse{
			Jobs: []*stateless.JobSummary{
				handlerutil.ConvertJobSummary(jobSummary, updateInfo),
			},
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}

	return nil
}

func (h *serviceHandler) ListJobUpdates(
	ctx context.Context,
	req *svc.ListJobUpdatesRequest) (*svc.ListJobUpdatesResponse, error) {
	return &svc.ListJobUpdatesResponse{}, nil
}

func (h *serviceHandler) GetReplaceJobDiff(
	ctx context.Context,
	req *svc.GetReplaceJobDiffRequest,
) (resp *svc.GetReplaceJobDiffResponse, err error) {
	defer func() {
		jobID := req.GetJobId().GetValue()
		entityVersion := req.GetVersion().GetValue()

		if err != nil {
			log.WithField("job_id", jobID).
				WithField("entity_version", entityVersion).
				WithError(err).
				Info("JobSVC.GetReplaceJobDifffailed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("job_id", jobID).
			WithField("entity_version", entityVersion).
			Debug("JobSVC.GetReplaceJobDiff succeeded")
	}()

	jobUUID := uuid.Parse(req.GetJobId().GetValue())
	if jobUUID == nil {
		return nil, yarpcerrors.InvalidArgumentErrorf(
			"JobID must be of UUID format")
	}

	jobID := &peloton.JobID{Value: req.GetJobId().GetValue()}
	cachedJob := h.jobFactory.AddJob(jobID)
	jobRuntime, err := cachedJob.GetRuntime(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get job status")
	}

	if err := cachedJob.ValidateEntityVersion(
		ctx,
		req.GetVersion(),
	); err != nil {
		return nil, err
	}

	jobConfig, err := handlerutil.ConvertJobSpecToJobConfig(req.GetSpec())
	if err != nil {
		return nil, err
	}

	prevJobConfig, _, err := h.jobStore.GetJobConfigWithVersion(
		ctx,
		jobID,
		jobRuntime.GetConfigurationVersion())
	if err != nil {
		return nil, errors.Wrap(err, "failed to get previous configuration")
	}

	if err := validateJobConfigUpdate(prevJobConfig, jobConfig); err != nil {
		return nil, err
	}

	added, updated, removed, unchanged, err :=
		cached.GetInstancesToProcessForUpdate(
			ctx,
			jobID,
			prevJobConfig,
			jobConfig,
			h.taskStore,
		)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get configuration difference")
	}

	return &svc.GetReplaceJobDiffResponse{
		InstancesAdded:     convertInstanceIDListToInstanceRange(added),
		InstancesRemoved:   convertInstanceIDListToInstanceRange(removed),
		InstancesUpdated:   convertInstanceIDListToInstanceRange(updated),
		InstancesUnchanged: convertInstanceIDListToInstanceRange(unchanged),
	}, nil
}

func (h *serviceHandler) RefreshJob(
	ctx context.Context,
	req *svc.RefreshJobRequest) (resp *svc.RefreshJobResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Warn("JobSVC.RefreshJob failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("request", req).
			WithField("response", resp).
			Info("JobSVC.RefreshJob succeeded")
	}()

	if !h.candidate.IsLeader() {
		return nil,
			yarpcerrors.UnavailableErrorf("JobSVC.RefreshJob is not supported on non-leader")
	}

	pelotonJobID := &peloton.JobID{Value: req.GetJobId().GetValue()}

	jobConfig, configAddOn, err := h.jobStore.GetJobConfig(ctx, pelotonJobID)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get job config")
	}

	jobRuntime, err := h.jobStore.GetJobRuntime(ctx, pelotonJobID)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get job runtime")
	}

	cachedJob := h.jobFactory.AddJob(pelotonJobID)
	cachedJob.Update(ctx, &pbjob.JobInfo{
		Config:  jobConfig,
		Runtime: jobRuntime,
	}, configAddOn,
		cached.UpdateCacheOnly)
	h.goalStateDriver.EnqueueJob(pelotonJobID, time.Now())
	return &svc.RefreshJobResponse{}, nil
}

func (h *serviceHandler) GetJobCache(
	ctx context.Context,
	req *svc.GetJobCacheRequest) (resp *svc.GetJobCacheResponse, err error) {
	defer func() {
		if err != nil {
			log.WithField("request", req).
				WithError(err).
				Warn("JobSVC.GetJobCache failed")
			err = handlerutil.ConvertToYARPCError(err)
			return
		}

		log.WithField("request", req).
			WithField("response", resp).
			Debug("JobSVC.GetJobCache succeeded")
	}()

	cachedJob := h.jobFactory.GetJob(&peloton.JobID{Value: req.GetJobId().GetValue()})
	if cachedJob == nil {
		return nil,
			yarpcerrors.NotFoundErrorf("job not found in cache")
	}

	runtime, err := cachedJob.GetRuntime(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get job runtime")
	}

	config, err := cachedJob.GetConfig(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "fail to get job config")
	}

	var cachedWorkflow cached.Update
	if len(runtime.GetUpdateID().GetValue()) > 0 {
		cachedWorkflow = cachedJob.GetWorkflow(runtime.GetUpdateID())
	}

	status := convertCacheToJobStatus(runtime)
	status.WorkflowStatus = convertCacheToWorkflowStatus(cachedWorkflow)

	return &svc.GetJobCacheResponse{
		Spec:   convertCacheJobConfigToJobSpec(config),
		Status: status,
	}, nil
}

func validateJobConfigUpdate(
	prevJobConfig *pbjob.JobConfig,
	newJobConfig *pbjob.JobConfig,
) error {
	// job type is immutable
	if newJobConfig.GetType() != prevJobConfig.GetType() {
		return yarpcerrors.InvalidArgumentErrorf("job type is immutable")
	}

	// resource pool identifier is immutable
	if newJobConfig.GetRespoolID().GetValue() !=
		prevJobConfig.GetRespoolID().GetValue() {
		return yarpcerrors.InvalidArgumentErrorf(
			"resource pool identifier is immutable")
	}

	return nil
}

func convertCacheJobConfigToJobSpec(config jobmgrcommon.JobConfig) *stateless.JobSpec {
	result := &stateless.JobSpec{}
	// set the fields used by both job config and cached job config
	result.InstanceCount = config.GetInstanceCount()
	result.RespoolId = &v1alphapeloton.ResourcePoolID{
		Value: config.GetRespoolID().GetValue(),
	}
	if config.GetSLA() != nil {
		result.Sla = &stateless.SlaSpec{
			Priority:                    config.GetSLA().GetPriority(),
			Preemptible:                 config.GetSLA().GetPreemptible(),
			Revocable:                   config.GetSLA().GetRevocable(),
			MaximumUnavailableInstances: config.GetSLA().GetMaximumUnavailableInstances(),
		}
	}
	result.Revision = &v1alphapeloton.Revision{
		Version:   config.GetChangeLog().GetVersion(),
		CreatedAt: config.GetChangeLog().GetCreatedAt(),
		UpdatedAt: config.GetChangeLog().GetUpdatedAt(),
		UpdatedBy: config.GetChangeLog().GetUpdatedBy(),
	}

	if _, ok := config.(*pbjob.JobConfig); ok {
		// TODO: set the rest of the fields in result
		// if the config passed in is a full config
	}

	return result
}

func convertCacheToJobStatus(
	runtime *pbjob.RuntimeInfo,
) *stateless.JobStatus {
	result := &stateless.JobStatus{}
	result.Revision = &v1alphapeloton.Revision{
		Version:   runtime.GetRevision().GetVersion(),
		CreatedAt: runtime.GetRevision().GetCreatedAt(),
		UpdatedAt: runtime.GetRevision().GetUpdatedAt(),
		UpdatedBy: runtime.GetRevision().GetUpdatedBy(),
	}
	result.State = stateless.JobState(runtime.GetState())
	result.CreationTime = runtime.GetCreationTime()
	result.PodStats = runtime.TaskStats
	result.DesiredState = stateless.JobState(runtime.GetGoalState())
	result.Version = jobutil.GetJobEntityVersion(
		runtime.GetConfigurationVersion(),
		runtime.GetWorkflowVersion())
	return result
}

func convertCacheToWorkflowStatus(
	cachedWorkflow cached.Update,
) *stateless.WorkflowStatus {
	workflowStatus := &stateless.WorkflowStatus{}
	workflowStatus.Type = stateless.WorkflowType(cachedWorkflow.GetWorkflowType())
	workflowStatus.State = stateless.WorkflowState(cachedWorkflow.GetState().State)
	workflowStatus.NumInstancesCompleted = uint32(len(cachedWorkflow.GetInstancesDone()))
	workflowStatus.NumInstancesFailed = uint32(len(cachedWorkflow.GetInstancesFailed()))
	workflowStatus.NumInstancesRemaining =
		uint32(len(cachedWorkflow.GetGoalState().Instances) -
			len(cachedWorkflow.GetInstancesDone()) -
			len(cachedWorkflow.GetInstancesFailed()))
	workflowStatus.InstancesCurrent = cachedWorkflow.GetInstancesCurrent()
	workflowStatus.PrevVersion = jobutil.GetPodEntityVersion(cachedWorkflow.GetState().JobVersion)
	workflowStatus.Version = jobutil.GetPodEntityVersion(cachedWorkflow.GetGoalState().JobVersion)
	return workflowStatus
}

// validateResourcePoolForJobCreation validates the resource pool before submitting job
func (h *serviceHandler) validateResourcePoolForJobCreation(
	ctx context.Context,
	respoolID *v1alphapeloton.ResourcePoolID,
) (*respool.ResourcePoolPath, error) {
	if respoolID == nil {
		return nil, errNullResourcePoolID
	}

	if respoolID.GetValue() == common.RootResPoolID {
		return nil, errRootResourcePoolID
	}

	request := &respool.GetRequest{
		Id: &peloton.ResourcePoolID{Value: respoolID.GetValue()},
	}
	response, err := h.respoolClient.GetResourcePool(ctx, request)
	if err != nil {
		return nil, err
	}

	if response.GetPoolinfo().GetId() == nil ||
		response.GetPoolinfo().GetId().GetValue() != respoolID.GetValue() {
		return nil, errResourcePoolNotFound
	}

	if len(response.GetPoolinfo().GetChildren()) > 0 {
		return nil, errNonLeafResourcePool
	}

	return response.GetPoolinfo().GetPath(), nil
}

// validateSecretsAndConfig checks the secrets for input sanity and makes sure
// that config does not contain any existing secret volumes because that is
// not supported.
func (h *serviceHandler) validateSecretsAndConfig(
	spec *stateless.JobSpec, secrets []*v1alphapeloton.Secret) error {
	// validate secrets payload for input sanity
	if len(secrets) == 0 {
		return nil
	}

	config, err := handlerutil.ConvertJobSpecToJobConfig(spec)
	if err != nil {
		return err
	}
	// make sure that config doesn't have any secret volumes
	if util.ConfigHasSecretVolumes(config.GetDefaultConfig()) {
		return yarpcerrors.InvalidArgumentErrorf(
			"adding secret volumes directly in config is not allowed",
		)
	}

	if !h.jobSvcCfg.EnableSecrets && len(secrets) > 0 {
		return yarpcerrors.InvalidArgumentErrorf(
			"secrets not enabled in cluster",
		)
	}
	for _, secret := range secrets {
		if secret.GetPath() == "" {
			return yarpcerrors.InvalidArgumentErrorf(
				"secret does not have a path")
		}
		// Validate that secret is base64 encoded
		_, err := base64.StdEncoding.DecodeString(
			string(secret.GetValue().GetData()))
		if err != nil {
			return yarpcerrors.InvalidArgumentErrorf(
				"failed to decode secret with error: %v", err,
			)
		}
	}
	return nil
}

// validateMesosContainerizerForSecrets returns error if default config doesn't
// use mesos containerizer. Secrets will be common for all instances in a job.
// They will be a part of default container config. This means that if a job is
// created with secrets, we will ensure that the job also has a default config
// with mesos containerizer. The secrets will be used by all tasks in that job
// and all tasks must use mesos containerizer for processing secrets.
// We will not enforce that instance config has mesos containerizer and let
// instance config override this to keep with existing convention.
func validateMesosContainerizerForSecrets(jobSpec *stateless.JobSpec) error {
	// make sure that default config uses mesos containerizer
	for _, container := range jobSpec.GetDefaultSpec().GetContainers() {
		if container.GetContainer().GetType() != mesos.ContainerInfo_MESOS {
			return yarpcerrors.InvalidArgumentErrorf(
				"container type %v does not match %v",
				jobSpec.GetDefaultSpec().GetContainers()[0].GetContainer().GetType(),
				mesos.ContainerInfo_MESOS,
			)
		}
	}
	return nil
}

// handleCreateSecrets handles secrets to be added at the time of creating a job
func (h *serviceHandler) handleCreateSecrets(
	ctx context.Context, jobID string,
	spec *stateless.JobSpec, secrets []*v1alphapeloton.Secret,
) error {
	// if there are no secrets in the request,
	// job create doesn't need to handle secrets
	if len(secrets) == 0 {
		return nil
	}
	// Make sure that the default config is using Mesos containerizer
	if err := validateMesosContainerizerForSecrets(spec); err != nil {
		return err
	}
	// for each secret, store it in DB and add a secret volume to defaultconfig
	return h.addSecretsToDBAndConfig(ctx, jobID, spec, secrets, false)
}

func (h *serviceHandler) addSecretsToDBAndConfig(
	ctx context.Context, jobID string, jobSpec *stateless.JobSpec,
	secrets []*v1alphapeloton.Secret, update bool) error {
	// for each secret, store it in DB and add a secret volume to defaultconfig
	for _, secret := range handlerutil.ConvertV1SecretsToV0Secrets(secrets) {
		if secret.GetId().GetValue() == "" {
			secret.Id = &peloton.SecretID{
				Value: uuid.New(),
			}
			log.WithField("job_id", secret.GetId().GetValue()).
				Info("Genarating UUID for empty secret ID")
		}
		// store secret in DB
		if update {
			if err := h.secretStore.UpdateSecret(ctx, secret); err != nil {
				return err
			}
		} else {
			if err := h.secretStore.CreateSecret(ctx, secret, &peloton.JobID{Value: jobID}); err != nil {
				return err
			}
		}
		// Add volume/secret to default container config with this secret
		// Use secretID instead of secret data when storing as
		// part of default config in DB.
		// This is done to prevent secrets leaks via logging/API etc.
		// At the time of task launch, launcher will read the
		// secret by secret-id and replace it by secret data.
		for _, container := range jobSpec.GetDefaultSpec().GetContainers() {
			container.GetContainer().Volumes =
				append(container.GetContainer().Volumes,
					util.CreateSecretVolume(secret.GetPath(),
						secret.GetId().GetValue()),
				)
		}
	}
	return nil
}

func convertInstanceIDListToInstanceRange(instIDs []uint32) []*pod.InstanceIDRange {
	var instanceIDRange []*pod.InstanceIDRange
	var instanceRange *pod.InstanceIDRange
	var prevInstID uint32

	instIDSortLess := func(i, j int) bool {
		return instIDs[i] < instIDs[j]
	}

	sort.Slice(instIDs, instIDSortLess)

	for _, instID := range instIDs {
		if instanceRange == nil {
			// create a new range
			instanceRange = &pod.InstanceIDRange{
				From: instID,
			}
		} else {
			// range already exists
			if instID != prevInstID+1 {
				// finish the previous range and start a new one
				instanceRange.To = prevInstID
				instanceIDRange = append(instanceIDRange, instanceRange)
				instanceRange = &pod.InstanceIDRange{
					From: instID,
				}
			}
		}
		prevInstID = instID
	}

	// finish the last instance range
	if instanceRange != nil {
		instanceRange.To = prevInstID
		instanceIDRange = append(instanceIDRange, instanceRange)
	}
	return instanceIDRange
}
