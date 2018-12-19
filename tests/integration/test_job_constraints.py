import pytest
from job import IntegrationTestConfig, Job
from peloton_client.pbgen.peloton.api.v0.task import task_pb2


# Mark test module so that we can run tests by tags
pytestmark = [pytest.mark.default, pytest.mark.job]


# Start a job with 4 instances with hostlimit:1
# Since we have 3 mesos agents , 3 tasks should start running on different
# hosts and have 1 task PENDING since there's no other host to satisfy the
# constraint.
def test__host_limit():
    job = Job(job_file='test_stateless_job_host_limit_1.yaml',
              config=IntegrationTestConfig(max_retry_attempts=100,
                                           sleep_time_sec=2))
    job.create()
    job.wait_for_state(goal_state='RUNNING')

    # All running tasks should have different hosts
    def different_hosts_for_running_tasks():
        hosts = set()
        num_running, num_pending = 0, 0
        tasks = job.list_tasks().value
        for id, t in tasks.items():
            if t.runtime.state == task_pb2.TaskState.Value('RUNNING'):
                num_running = num_running + 1
                hosts.add(t.runtime.host)
            if t.runtime.state == task_pb2.TaskState.Value('PENDING'):
                num_pending = num_pending + 1

        # number of running tasks should be equal to the size of the hosts set
        # there should be 1 task in PENDING
        return len(hosts) == num_running and num_pending == 1

    job.wait_for_condition(different_hosts_for_running_tasks)

    job.stop()
    job.wait_for_state(goal_state='KILLED')