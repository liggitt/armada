package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gogo/protobuf/types"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/G-Research/armada/internal/armada/configuration"
	"github.com/G-Research/armada/internal/armada/permissions"
	"github.com/G-Research/armada/internal/armada/repository"
	"github.com/G-Research/armada/internal/common/auth/authorization"
	"github.com/G-Research/armada/internal/common/util"
	"github.com/G-Research/armada/internal/common/validation"
	"github.com/G-Research/armada/pkg/api"
	"github.com/G-Research/armada/pkg/client/queue"
)

type SubmitServer struct {
	permissions              authorization.PermissionChecker
	jobRepository            repository.JobRepository
	queueRepository          repository.QueueRepository
	eventStore               repository.EventStore
	schedulingInfoRepository repository.SchedulingInfoRepository
	cancelJobsBatchSize      int
	queueManagementConfig    *configuration.QueueManagementConfig
	schedulingConfig         *configuration.SchedulingConfig
}

func NewSubmitServer(
	permissions authorization.PermissionChecker,
	jobRepository repository.JobRepository,
	queueRepository repository.QueueRepository,
	eventStore repository.EventStore,
	schedulingInfoRepository repository.SchedulingInfoRepository,
	cancelJobsBatchSize int,
	queueManagementConfig *configuration.QueueManagementConfig,
	schedulingConfig *configuration.SchedulingConfig,
) *SubmitServer {

	return &SubmitServer{
		permissions:              permissions,
		jobRepository:            jobRepository,
		queueRepository:          queueRepository,
		eventStore:               eventStore,
		schedulingInfoRepository: schedulingInfoRepository,
		cancelJobsBatchSize:      cancelJobsBatchSize,
		queueManagementConfig:    queueManagementConfig,
		schedulingConfig:         schedulingConfig}
}

func (server *SubmitServer) GetQueueInfo(ctx context.Context, req *api.QueueInfoRequest) (*api.QueueInfo, error) {
	q, err := server.queueRepository.GetQueue(req.Name)
	var expected *repository.ErrQueueNotFound
	if errors.Is(err, expected) {
		return nil, status.Errorf(codes.NotFound, "[GetQueueInfo] Queue %s does not exist", req.Name)
	}
	if err != nil {
		return nil, err
	}

	err = checkPermission(server.permissions, ctx, permissions.WatchAllEvents)
	var globalPermErr *ErrNoPermission
	if errors.As(err, &globalPermErr) {
		err = checkQueuePermission(server.permissions, ctx, q, permissions.WatchEvents, queue.PermissionVerbWatch)
		var queuePermErr *ErrNoPermission
		if errors.As(err, &queuePermErr) {
			return nil, status.Errorf(codes.PermissionDenied,
				"[GetQueueInfo] error getting info for queue %s: %s", req.Name, MergePermissionErrors(globalPermErr, queuePermErr))
		} else if err != nil {
			return nil, status.Errorf(codes.Unavailable, "[GetQueueInfo] error checking permissions: %s", err)
		}
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[GetQueueInfo] error checking permissions: %s", err)
	}

	jobSets, e := server.jobRepository.GetQueueActiveJobSets(req.Name)
	if e != nil {
		return nil, status.Errorf(codes.Unavailable, "[GetQueueInfo] error getting job sets for queue %s: %s", req.Name, err)
	}

	return &api.QueueInfo{
		Name:          req.Name,
		ActiveJobSets: jobSets,
	}, nil
}

func (server *SubmitServer) GetQueue(ctx context.Context, req *api.QueueGetRequest) (*api.Queue, error) {
	queue, err := server.queueRepository.GetQueue(req.Name)
	var e *repository.ErrQueueNotFound
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.NotFound, "[GetQueue] error: %s", err)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[GetQueue] error getting queue %q: %s", req.Name, err)
	}
	return queue.ToAPI(), nil
}

func (server *SubmitServer) CreateQueue(ctx context.Context, request *api.Queue) (*types.Empty, error) {
	err := checkPermission(server.permissions, ctx, permissions.CreateQueue)
	var ep *ErrNoPermission
	if errors.As(err, &ep) {
		return nil, status.Errorf(codes.PermissionDenied, "[CreateQueue] error creating queue %s: %s", request.Name, ep)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[CreateQueue] error checking permissions: %s", err)
	}

	if len(request.UserOwners) == 0 {
		principal := authorization.GetPrincipal(ctx)
		request.UserOwners = []string{principal.GetName()}
	}

	queue, err := queue.NewQueue(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[CreateQueue] error validating queue: %s", err)
	}

	err = server.queueRepository.CreateQueue(queue)
	var eq *repository.ErrQueueAlreadyExists
	if errors.As(err, &eq) {
		return nil, status.Errorf(codes.AlreadyExists, "[CreateQueue] error creating queue: %s", err)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[CreateQueue] error creating queue: %s", err)
	}

	return &types.Empty{}, nil
}

func (server *SubmitServer) UpdateQueue(ctx context.Context, request *api.Queue) (*types.Empty, error) {
	err := checkPermission(server.permissions, ctx, permissions.CreateQueue)
	var ep *ErrNoPermission
	if errors.As(err, &ep) {
		return nil, status.Errorf(codes.PermissionDenied, "[UpdateQueue] error updating queue %s: %s", request.Name, ep)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[UpdateQueue] error checking permissions: %s", err)
	}

	queue, err := queue.NewQueue(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[UpdateQueue] error: %s", err)
	}

	err = server.queueRepository.UpdateQueue(queue)
	var e *repository.ErrQueueNotFound
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.NotFound, "[UpdateQueue] error: %s", err)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[UpdateQueue] error getting queue %q: %s", queue.Name, err)
	}

	return &types.Empty{}, nil
}

func (server *SubmitServer) DeleteQueue(ctx context.Context, request *api.QueueDeleteRequest) (*types.Empty, error) {
	err := checkPermission(server.permissions, ctx, permissions.DeleteQueue)
	var ep *ErrNoPermission
	if errors.As(err, &ep) {
		return nil, status.Errorf(codes.PermissionDenied, "[DeleteQueue] error deleting queue %s: %s", request.Name, ep)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[DeleteQueue] error checking permissions: %s", err)
	}

	active, err := server.jobRepository.GetQueueActiveJobSets(request.Name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[DeleteQueue] error getting active job sets for queue %s: %s", request.Name, err)
	}
	if len(active) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "[DeleteQueue] error deleting queue %s: queue is not empty", request.Name)
	}

	err = server.queueRepository.DeleteQueue(request.Name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[DeleteQueue] error deleting queue %s: %s", request.Name, err)
	}

	return &types.Empty{}, nil
}

func (server *SubmitServer) SubmitJobs(ctx context.Context, req *api.JobSubmitRequest) (*api.JobSubmitResponse, error) {
	principal := authorization.GetPrincipal(ctx)

	q, err := server.getQueueOrCreate(ctx, req.Queue)
	if err != nil {
		return nil, err
	}

	err = checkPermission(server.permissions, ctx, permissions.SubmitAnyJobs)
	var globalPermErr *ErrNoPermission
	if errors.As(err, &globalPermErr) {
		err = checkQueuePermission(server.permissions, ctx, q, permissions.SubmitJobs, queue.PermissionVerbSubmit)
		var queuePermErr *ErrNoPermission
		if errors.As(err, &queuePermErr) {
			return nil, status.Errorf(codes.PermissionDenied,
				"[SubmitJobs] error submitting job in queue %s: %s", req.Queue, MergePermissionErrors(globalPermErr, queuePermErr))
		} else if err != nil {
			return nil, status.Errorf(codes.Unavailable, "[SubmitJobs] error checking permissions: %s", err)
		}
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[SubmitJobs] error checking permissions: %s", err)
	}

	principalSubject := queue.PermissionSubject{
		Name: principal.GetName(),
		Kind: queue.PermissionSubjectKindUser,
	}

	groups := []string{}
	if !q.HasPermission(principalSubject, queue.PermissionVerbSubmit) {
		for _, subject := range queue.NewPermissionSubjectsFromOwners(nil, principal.GetGroupNames()) {
			if q.HasPermission(subject, queue.PermissionVerbSubmit) {
				groups = append(groups, subject.Name)
			}
		}
	}

	jobs, e := server.createJobs(req, principal.GetName(), groups)
	if e != nil {
		reqJson, _ := json.Marshal(req)
		return nil, status.Errorf(codes.InvalidArgument, "[SubmitJobs] Error submitting job %s for user %s: %v", reqJson, principal.GetName(), e)
	}

	// Check if the job would fit on any executor,
	// to avoid having users wait for a job that may never be scheduled
	allClusterSchedulingInfo, err := server.schedulingInfoRepository.GetClusterSchedulingInfo()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[SubmitJobs] error getting scheduling info: %s", err)
	}

	err = validateJobsCanBeScheduled(jobs, allClusterSchedulingInfo)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[SubmitJobs] error submitting jobs for user %s: %s", principal.GetName(), err)
	}

	// Create events marking the jobs as submitted
	err = reportSubmitted(server.eventStore, jobs)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "[SubmitJobs] error getting submitted report: %s", err)
	}

	// Submit the jobs by writing them to the database
	submissionResults, err := server.jobRepository.AddJobs(jobs)
	if err != nil {
		jobFailures := createJobFailuresWithReason(jobs, fmt.Sprintf("Failed to save job in Armada: %s", e))
		reportErr := reportFailed(server.eventStore, "", jobFailures)
		if reportErr != nil {
			return nil, status.Errorf(codes.Internal, "[SubmitJobs] error reporting failure event: %v", reportErr)
		}
		return nil, status.Errorf(codes.Aborted, "[SubmitJobs] error saving jobs in Armada: %s", err)
	}

	// Create the response to send to the client
	result := &api.JobSubmitResponse{
		JobResponseItems: make([]*api.JobSubmitResponseItem, 0, len(submissionResults)),
	}

	var createdJobs []*api.Job
	var jobFailures []*jobFailure
	var doubleSubmits []*repository.SubmitJobResult

	for i, submissionResult := range submissionResults {
		jobResponse := &api.JobSubmitResponseItem{JobId: submissionResult.JobId}

		if submissionResult.Error != nil {
			jobResponse.Error = submissionResult.Error.Error()
			jobFailures = append(jobFailures, &jobFailure{
				job:    jobs[i],
				reason: fmt.Sprintf("Failed to save job in Armada: %s", submissionResult.Error.Error()),
			})
		} else if submissionResult.DuplicateDetected {
			doubleSubmits = append(doubleSubmits, submissionResult)
		} else {
			createdJobs = append(createdJobs, jobs[i])
		}

		result.JobResponseItems = append(result.JobResponseItems, jobResponse)
	}

	err = reportFailed(server.eventStore, "", jobFailures)
	if err != nil {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error reporting failed jobs: %s", err))
	}

	err = reportDuplicateDetected(server.eventStore, doubleSubmits)
	if err != nil {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error reporting duplicate jobs: %s", err))
	}

	err = reportQueued(server.eventStore, createdJobs)
	if err != nil {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error reporting queued jobs: %s", err))
	}

	if len(jobFailures) > 0 {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error submitting some or all jobs: %s", err))
	}

	return result, nil
}

// CancelJobs cancels jobs identified by the request.
// If the request contains a job ID, only the job with that ID is cancelled.
// If the request contains a queue name and a job set ID, all jobs matching those are cancelled.
func (server *SubmitServer) CancelJobs(ctx context.Context, request *api.JobCancelRequest) (*api.CancellationResult, error) {
	if request.JobId != "" {
		return server.cancelJobsById(ctx, request.JobId)
	} else if request.JobSetId != "" && request.Queue != "" {
		return server.cancelJobsByQueueAndSet(ctx, request.Queue, request.JobSetId)
	}
	return nil, status.Errorf(codes.InvalidArgument, "[CancelJobs] specify either job ID or both queue name and job set ID")
}

// cancels a job with a given ID
func (server *SubmitServer) cancelJobsById(ctx context.Context, jobId string) (*api.CancellationResult, error) {
	jobs, err := server.jobRepository.GetExistingJobsByIds([]string{jobId})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[cancelJobsById] error getting job with ID %s: %s", jobId, err)
	}
	if len(jobs) != 1 {
		return nil, status.Errorf(codes.Internal, "[cancelJobsById] error getting job with ID %s: expected exactly one result, but got %v", jobId, jobs)
	}

	result, err := server.cancelJobs(ctx, jobs)
	var e *ErrNoPermission
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.PermissionDenied, "[cancelJobsById] error canceling job with ID %s: %s", jobId, e)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[cancelJobsById] error checking permissions: %s", err)
	}

	return result, nil
}

// cancels all jobs part of a particular job set and queue
func (server *SubmitServer) cancelJobsByQueueAndSet(ctx context.Context, queue string, jobSetId string) (*api.CancellationResult, error) {
	ids, err := server.jobRepository.GetActiveJobIds(queue, jobSetId)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[cancelJobsBySetAndQueue] error getting job IDs: %s", err)
	}

	// Split IDs into batches and process one batch at a time
	// To reduce the number of jobs stored in memory
	batches := util.Batch(ids, server.cancelJobsBatchSize)
	cancelledIds := []string{}
	for _, batch := range batches {
		jobs, err := server.jobRepository.GetExistingJobsByIds(batch)
		if err != nil {
			result := &api.CancellationResult{CancelledIds: cancelledIds}
			return result, status.Errorf(codes.Internal, "[cancelJobsBySetAndQueue] error getting jobs: %s", err)
		}

		result, err := server.cancelJobs(ctx, jobs)
		var e *ErrNoPermission
		if errors.As(err, &e) {
			return nil, status.Errorf(codes.PermissionDenied, "[cancelJobsBySetAndQueue] error canceling jobs: %s", e)
		} else if err != nil {
			result := &api.CancellationResult{CancelledIds: cancelledIds}
			return result, status.Errorf(codes.Unavailable, "[cancelJobsBySetAndQueue] error checking permissions: %s", err)
		}
		cancelledIds = append(cancelledIds, result.CancelledIds...)

		// TODO I think the right way to do this is to include a timeout with the call to Redis
		// Then, we can check for a deadline exceeded error here
		if util.CloseToDeadline(ctx, time.Second*1) {
			result := &api.CancellationResult{CancelledIds: cancelledIds}
			return result, status.Errorf(codes.DeadlineExceeded, "[cancelJobsBySetAndQueue] deadline exceeded")
		}
	}

	return &api.CancellationResult{CancelledIds: cancelledIds}, nil
}

func (server *SubmitServer) cancelJobs(ctx context.Context, jobs []*api.Job) (*api.CancellationResult, error) {
	principal := authorization.GetPrincipal(ctx)

	err := server.checkCancelPerms(ctx, jobs)
	if err != nil {
		return nil, err
	}

	err = reportJobsCancelling(server.eventStore, principal.GetName(), jobs)
	if err != nil {
		return nil, fmt.Errorf("[cancelJobs] error reporting jobs marked as cancelled: %w", err)
	}

	deletionResult, err := server.jobRepository.DeleteJobs(jobs)
	if err != nil {
		return nil, fmt.Errorf("[cancelJobs] error deleting jobs: %w", err)
	}
	cancelled := []*api.Job{}
	cancelledIds := []string{}
	for job, err := range deletionResult {
		if err != nil {
			log.Errorf("[cancelJobs] error cancelling job with ID %s: %s", job.Id, err)
		} else {
			cancelled = append(cancelled, job)
			cancelledIds = append(cancelledIds, job.Id)
		}
	}

	err = reportJobsCancelled(server.eventStore, principal.GetName(), cancelled)
	if err != nil {
		return nil, fmt.Errorf("[cancelJobs] error reporting job cancellation: %w", err)
	}

	return &api.CancellationResult{CancelledIds: cancelledIds}, nil
}

func (server *SubmitServer) checkCancelPerms(ctx context.Context, jobs []*api.Job) error {
	queueNames := make(map[string]struct{})
	for _, job := range jobs {
		queueNames[job.Queue] = struct{}{}
	}
	for queueName := range queueNames {
		q, err := server.queueRepository.GetQueue(queueName)
		if err != nil {
			return err
		}

		err = checkPermission(server.permissions, ctx, permissions.CancelAnyJobs)
		var globalPermErr *ErrNoPermission
		if errors.As(err, &globalPermErr) {
			err = checkQueuePermission(server.permissions, ctx, q, permissions.CancelJobs, queue.PermissionVerbCancel)
			var queuePermErr *ErrNoPermission
			if errors.As(err, &queuePermErr) {
				return MergePermissionErrors(globalPermErr, queuePermErr)
			} else if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

// ReprioritizeJobs updates the priority of one of more jobs.
// Returns a map from job ID to any error (or nil if the call succeeded).
func (server *SubmitServer) ReprioritizeJobs(ctx context.Context, request *api.JobReprioritizeRequest) (*api.JobReprioritizeResponse, error) {
	var jobs []*api.Job
	if len(request.JobIds) > 0 {
		existingJobs, err := server.jobRepository.GetExistingJobsByIds(request.JobIds)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error getting jobs by ID: %s", err)
		}
		jobs = existingJobs
	} else if request.Queue != "" && request.JobSetId != "" {
		ids, err := server.jobRepository.GetActiveJobIds(request.Queue, request.JobSetId)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error getting job IDs for queue %s and job set %s: %s", request.Queue, request.JobSetId, err)
		}

		existingJobs, err := server.jobRepository.GetExistingJobsByIds(ids)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error getting jobs for queue %s and job set %s: %s", request.Queue, request.JobSetId, err)
		}
		jobs = existingJobs
	}

	err := server.checkReprioritizePerms(ctx, jobs)
	var e *ErrNoPermission
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.PermissionDenied, "[ReprioritizeJobs] error: %s", e)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error checking permissions: %s", err)
	}

	principalName := authorization.GetPrincipal(ctx).GetName()
	err = reportJobsReprioritizing(server.eventStore, principalName, jobs, request.NewPriority)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error reporting job re-prioritization: %s", err)
	}

	jobIds := []string{}
	for _, job := range jobs {
		jobIds = append(jobIds, job.Id)
	}
	results, err := server.reprioritizeJobs(jobIds, request.NewPriority, principalName)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error re-prioritizing jobs: %s", err)
	}

	return &api.JobReprioritizeResponse{ReprioritizationResults: results}, nil
}

func (server *SubmitServer) reprioritizeJobs(jobIds []string, newPriority float64, principalName string) (map[string]string, error) {

	// TODO There's a bug here.
	// The function passed to UpdateJobs is called under an optimistic lock.
	// If the jobs to be updated are mutated by another thread concurrently,
	// the changes are not written to Redis. However, this function has side effects
	// (creating reprioritized events) that would not be rolled back.
	updateJobResults, err := server.jobRepository.UpdateJobs(jobIds, func(jobs []*api.Job) {
		for _, job := range jobs {
			job.Priority = newPriority
		}
		err := server.reportReprioritizedJobEvents(jobs, newPriority, principalName)
		if err != nil {
			log.Warnf("Failed to report events for reprioritize of jobs %s: %v", strings.Join(jobIds, ", "), err)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("[reprioritizeJobs] error updating jobs: %s", err)
	}

	results := map[string]string{}
	for _, r := range updateJobResults {
		if r.Error == nil {
			results[r.JobId] = ""
		} else {
			results[r.JobId] = r.Error.Error()
		}
	}
	return results, nil
}

func (server *SubmitServer) reportReprioritizedJobEvents(reprioritizedJobs []*api.Job, newPriority float64, principalName string) error {
	err := reportJobsUpdated(server.eventStore, principalName, reprioritizedJobs)
	if err != nil {
		return fmt.Errorf("[reportReprioritizedJobEvents] error reporting jobs updated: %w", err)
	}

	err = reportJobsReprioritized(server.eventStore, principalName, reprioritizedJobs, newPriority)
	if err != nil {
		return fmt.Errorf("[reportReprioritizedJobEvents] error reporting jobs reprioritized: %w", err)
	}

	return nil
}

func (server *SubmitServer) checkReprioritizePerms(ctx context.Context, jobs []*api.Job) error {
	queueNames := make(map[string]struct{})
	for _, job := range jobs {
		queueNames[job.Queue] = struct{}{}
	}
	for queueName := range queueNames {
		q, err := server.queueRepository.GetQueue(queueName)
		if err != nil {
			return err
		}

		err = checkPermission(server.permissions, ctx, permissions.ReprioritizeAnyJobs)
		var globalPermErr *ErrNoPermission
		if errors.As(err, &globalPermErr) {
			err = checkQueuePermission(server.permissions, ctx, q, permissions.ReprioritizeJobs, queue.PermissionVerbReprioritize)
			var queuePermErr *ErrNoPermission
			if errors.As(err, &queuePermErr) {
				return MergePermissionErrors(globalPermErr, queuePermErr)
			} else if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (server *SubmitServer) getQueueOrCreate(ctx context.Context, queueName string) (queue.Queue, error) {
	q, e := server.queueRepository.GetQueue(queueName)
	if e == nil {
		return q, nil
	}
	var expected *repository.ErrQueueNotFound

	if errors.As(e, &expected) {
		if !server.queueManagementConfig.AutoCreateQueues || !server.permissions.UserHasPermission(ctx, permissions.SubmitAnyJobs) {
			return queue.Queue{}, status.Errorf(codes.NotFound, "Queue %q not found", queueName)
		}

		principal := authorization.GetPrincipal(ctx)

		q = queue.Queue{
			Name:           queueName,
			PriorityFactor: server.queueManagementConfig.DefaultPriorityFactor,
			Permissions: []queue.Permissions{
				queue.NewPermissionsFromOwners([]string{principal.GetName()}, principal.GetGroupNames()),
			},
		}

		if err := server.queueRepository.CreateQueue(q); err != nil {
			return queue.Queue{}, status.Errorf(codes.Aborted, e.Error())
		}
		return q, nil
	}

	return queue.Queue{}, status.Errorf(codes.Unavailable, "Could not load queue %q: %s", queueName, e.Error())
}

// createJobs returns a list of objects representing the jobs in a JobSubmitRequest.
// This function validates the jobs in the request and the pod specs. in each job.
// If any job or pod in invalid, an error is returned.
func (server *SubmitServer) createJobs(request *api.JobSubmitRequest, owner string, ownershipGroups []string) ([]*api.Job, error) {
	return server.createJobsObjects(request, owner, ownershipGroups, time.Now, util.NewULID)
}

func (server *SubmitServer) createJobsObjects(request *api.JobSubmitRequest, owner string, ownershipGroups []string,
	getTime func() time.Time, getUlid func() string) ([]*api.Job, error) {
	jobs := make([]*api.Job, 0, len(request.JobRequestItems))

	if request.JobSetId == "" {
		return nil, fmt.Errorf("[createJobs] job set not specified")
	}

	if request.Queue == "" {
		return nil, fmt.Errorf("[createJobs] queue not specified")
	}

	for i, item := range request.JobRequestItems {

		if item.PodSpec != nil && len(item.PodSpecs) > 0 {
			return nil, fmt.Errorf("[createJobs] job %d in job set %s contains both podSpec and podSpecs, but may only contain either", i, request.JobSetId)
		}

		podSpecs := item.GetAllPodSpecs()
		if len(podSpecs) == 0 {
			return nil, fmt.Errorf("[createJobs] job %d in job set %s contains no podSpec or podSpecs", i, request.JobSetId)
		}

		if err := validation.ValidateJobSubmitRequestItem(item); err != nil {
			return nil, fmt.Errorf("[createJobs] error validating the %d-th job of job set %s: %w", i, request.JobSetId, err)
		}

		namespace := item.Namespace
		if namespace == "" {
			namespace = "default"
		}

		for j, podSpec := range item.GetAllPodSpecs() {
			if podSpec != nil {
				fillContainerRequestsAndLimits(podSpec.Containers)
			}
			server.applyDefaultsToPodSpec(podSpec)
			err := validation.ValidatePodSpec(podSpec, server.schedulingConfig)
			if err != nil {
				return nil, fmt.Errorf("[createJobs] error validating the %d-th pod pf the %d-th job of job set %s: %w", j, i, request.JobSetId, err)
			}

			// TODO: remove, RequiredNodeLabels is deprecated and will be removed in future versions
			for k, v := range item.RequiredNodeLabels {
				if podSpec.NodeSelector == nil {
					podSpec.NodeSelector = map[string]string{}
				}
				podSpec.NodeSelector[k] = v
			}
		}

		jobId := getUlid()
		enrichText(item.Labels, jobId)
		enrichText(item.Annotations, jobId)
		j := &api.Job{
			Id:       jobId,
			ClientId: item.ClientId,
			Queue:    request.Queue,
			JobSetId: request.JobSetId,

			Namespace:   namespace,
			Labels:      item.Labels,
			Annotations: item.Annotations,

			RequiredNodeLabels: item.RequiredNodeLabels,
			Ingress:            item.Ingress,
			Services:           item.Services,

			Priority: item.Priority,

			PodSpec:                  item.PodSpec,
			PodSpecs:                 item.PodSpecs,
			Created:                  getTime(), // Replaced with now for mocking unit test
			Owner:                    owner,
			QueueOwnershipUserGroups: ownershipGroups,
		}
		jobs = append(jobs, j)
	}

	return jobs, nil
}

func enrichText(labels map[string]string, jobId string) {
	for key, value := range labels {
		value := strings.ReplaceAll(value, "{{JobId}}", ` \z`) // \z cannot be entered manually, hence its use
		value = strings.ReplaceAll(value, "{JobId}", jobId)
		labels[key] = strings.ReplaceAll(value, ` \z`, "JobId")
	}
}

func (server *SubmitServer) applyDefaultsToPodSpec(spec *v1.PodSpec) {
	if spec == nil {
		return
	}

	// Add default resource requests and limits if missing
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Resources.Limits == nil {
			c.Resources.Limits = map[v1.ResourceName]resource.Quantity{}
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = map[v1.ResourceName]resource.Quantity{}
		}
		for res, val := range server.schedulingConfig.DefaultJobLimits {
			_, hasLimit := c.Resources.Limits[v1.ResourceName(res)]
			_, hasRequest := c.Resources.Limits[v1.ResourceName(res)]

			// TODO Should we check and apply these separately?
			if !hasLimit && !hasRequest {
				c.Resources.Requests[v1.ResourceName(res)] = val
				c.Resources.Limits[v1.ResourceName(res)] = val
			}
		}
	}

	// Each pod must have some default tolerations
	// Here, we add any that are missing
	podTolerations := make(map[string]v1.Toleration)
	for _, podToleration := range spec.Tolerations {
		podTolerations[podToleration.Key] = podToleration
	}
	for _, defaultToleration := range server.schedulingConfig.DefaultJobTolerations {
		podToleration, ok := podTolerations[defaultToleration.Key]
		if !ok || !defaultToleration.MatchToleration(&podToleration) {
			spec.Tolerations = append(spec.Tolerations, defaultToleration)
		}
	}
}

// fillContainerRequestAndLimits updates resource's requests/limits of container to match the value of
// limits/requests if the resource doesn't have requests/limits setup. If a Container specifies its own
// memory limit, but does not specify a memory request, assign a memory request that matches the limit.
// Similarly, if a Container specifies its own CPU limit, but does not specify a CPU request, automatically
// assigns a CPU request that matches the limit.
func fillContainerRequestsAndLimits(containers []v1.Container) {
	for index := range containers {
		if containers[index].Resources.Limits == nil {
			containers[index].Resources.Limits = v1.ResourceList{}
		}
		if containers[index].Resources.Requests == nil {
			containers[index].Resources.Requests = v1.ResourceList{}
		}

		for resourceName, quantity := range containers[index].Resources.Limits {
			if _, ok := containers[index].Resources.Requests[resourceName]; !ok {
				containers[index].Resources.Requests[resourceName] = quantity
			}
		}

		for resourceName, quantity := range containers[index].Resources.Requests {
			if _, ok := containers[index].Resources.Limits[resourceName]; !ok {
				containers[index].Resources.Limits[resourceName] = quantity
			}
		}
	}
}

func createJobFailuresWithReason(jobs []*api.Job, reason string) []*jobFailure {
	jobFailures := make([]*jobFailure, len(jobs), len(jobs))
	for i, job := range jobs {
		jobFailures[i] = &jobFailure{
			job:    job,
			reason: reason,
		}
	}
	return jobFailures
}
