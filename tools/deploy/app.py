from __future__ import absolute_import

from enum import Enum
from time import sleep, time

from aurora.api.constants import AURORA_EXECUTOR_NAME, ACTIVE_STATES
from aurora.api.ttypes import (
    JobKey,
    TaskConfig,
    JobConfiguration,
    ExecutorConfig,
    Container,
    DockerContainer,
    DockerParameter,
    Identity,
    ResponseCode,
    ScheduleStatus,
    TaskQuery,
    JobUpdateRequest,
    JobUpdateSettings,
    JobUpdateQuery,
    JobUpdateStatus,
    Range,
    Constraint,
    TaskConstraint,
    LimitConstraint,
    Resource,
)
from aurora.schema.thermos.schema_base import (
    Task as ThermosTask,
    Process as ThermosProcess,
    Resources,
)
from aurora.schema.aurora.base import (
    MesosJob as ThermosJob,
    HealthCheckConfig,
    HealthCheckerConfig,
    HttpHealthChecker
)

KB = 1024
MB = 1024 * KB
GB = 1024 * MB

AURORA_ROLE = 'peloton'
AURORA_ENVIRONMENT = 'production'
AURORA_USER = 'peloton'


MAX_WAIT_TIME_SECONDS = 300
WAIT_INTERVAL_SECONDS = 10


def combine_messages(response):
    """
    Combines the message found in the details of a response.

    :param response: response to extract messages from.
    :return: Messages from the details in the response, or an empty string if
        there were no messages.
    """
    return ', '.join([
        d.message or 'Unknown error' for d in (response.details or [])
    ])


class Role(Enum):
    """
    The role of a Peloton app instance
    """
    UNKNOWN = 1
    LEADER = 2
    FOLLOWER = 3
    ALL = 4


class Instance(object):
    """
    Representation of an instance of a Peloton application
    """

    def __init__(self, task, role):
        assert(task.assignedTask)
        self.instance_id = task.assignedTask.instanceId
        self.host = task.assignedTask.slaveHost
        self.state = task.status

        # TODO: query Peloton app endpoint to determine leader/follower role
        self.role = role


class App(object):
    """
    Representation of a Peloton application with multiple instances
    """

    def __init__(self, **kwargs):
        """
        Initializes a Peloton application
        """
        # Default attributes
        self.enable_debug_logging = False
        self.cpu_limit = 4.0
        self.mem_limit = 8 * GB
        self.disk_limit = 16 * GB

        for k, v in kwargs.iteritems():
            setattr(self, k, v)

        self.client = self.cluster.client

        if self.num_instances < 1:
            raise Exception('App %s has no instances' % self.name)

        self.job_key = JobKey(
            role=AURORA_ROLE,
            environment=AURORA_ENVIRONMENT,
            name=self.name,
        )

        # Generate the new job config for this app
        self.desired_job_config = self.get_desired_job_config()

        # Save current job config so that we can rollback to later
        self.current_job_config = self.get_current_job_config()

    def get_docker_params(self):
        """
        Returns the docker params for a given Peloton application
        """
        mesos_zk_path = 'zk://%s/%s' % (
            self.cluster.zookeeper, self.cluster.mesos_zk_path)

        env_vars = {
            'ENVIRONMENT': 'production',
            'CONFIG_DIR': './config',
            'APP': self.name,
            # TODO: fix Peloton code to only take self.cluster.mesos_zk_path
            'MESOS_ZK_PATH': mesos_zk_path,
            'ENABLE_DEBUG_LOGGING': self.enable_debug_logging,
            'ELECTION_ZK_SERVERS': self.cluster.zookeeper,
            'USE_CASSANDRA': self.cluster.cassandra is not None,
            'CASSANDRA_HOSTS': '\n'.join(self.cluster.cassandra),
            'CASSANDRA_STORE': self.cluster.name.replace('-', '_'),
            'CLUSTER': self.cluster.name,
            'DATACENTER': getattr(self.cluster, 'datacenter', ''),
        }

        params = [
            DockerParameter(name='env', value='%s=%s' % (key, val))
            for key, val in env_vars.iteritems()
        ]

        volumes = [
            ('/var/log/peloton', '/var/log/peloton', 'rw')
        ]

        params.extend(
            DockerParameter(name='volume', value='%s:%s:%s' % (src, dst, mode))
            for src, dst, mode in volumes
        )

        return params

    def get_docker_image(self):
        """
        Returns the docker image path for a Peloton app
        """
        return '%s/vendor/peloton:%s' % (
            self.cluster.docker_registry, self.cluster.version
        )

    def get_executor_config(self):
        """
        Returns the Thermos executor config for a Peloton app
        """

        host_logdir = '/var/log/peloton/%s' % self.name
        sandbox_logdir = '$MESOS_DIRECTORY/sandbox/.logs/%s/0' % self.name
        cmdline = ' && '.join([
            'rm -rf %s' % host_logdir,
            'ln -s %s %s' % (sandbox_logdir, host_logdir),
            '/bin/entrypoint.sh'
        ])
        entrypoint_process = ThermosProcess(
            name=self.name,
            cmdline=cmdline,
        )
        thermos_task = ThermosTask(
            name=self.name,
            processes=[entrypoint_process],
            resources=Resources(
                cpu=self.cpu_limit,
                ram=self.mem_limit,
                disk=self.disk_limit,
            ),
        )
        health_check_config = HealthCheckConfig(
            health_checker=HealthCheckerConfig(
                http=HttpHealthChecker(
                    endpoint='/health',
                    expected_response='OK',
                    expected_response_code=200
                ),
            ),
            initial_interval_secs=15,
            interval_secs=10,
            max_consecutive_failures=3,
            timeout_secs=1
        )
        thermos_job = ThermosJob(
            name=self.name,
            role=AURORA_ROLE,
            cluster=self.cluster.name,
            environment=AURORA_ENVIRONMENT,
            task=thermos_task,
            production=False,
            service=True,
            health_check_config=health_check_config,
        )
        executor_config = ExecutorConfig(
            name=AURORA_EXECUTOR_NAME,
            data=thermos_job.json_dumps()
        )
        return executor_config

    def get_desired_job_config(self):
        """
        Return the Aurora job configuration defined in Thrift API so that
        we can create a job via Aurora API.
        """

        # Add docker container
        container = Container(
            mesos=None,
            docker=DockerContainer(
                image=self.get_docker_image(),
                parameters=self.get_docker_params()
            )
        )

        host_limit = Constraint(
            name=self.cluster.constraint,
            constraint=TaskConstraint(
                limit=LimitConstraint(
                    limit=1,
                )
            )
        )

        task_config = TaskConfig(
            job=self.job_key,
            owner=Identity(user=AURORA_USER),
            isService=True,
            numCpus=self.cpu_limit,
            ramMb=self.mem_limit / MB,
            diskMb=self.disk_limit / MB,
            priority=0,
            maxTaskFailures=0,
            production=False,
            tier='preemptible',
            resources=set([
                Resource(numCpus=self.cpu_limit),
                Resource(ramMb=self.mem_limit / MB),
                Resource(diskMb=self.disk_limit / MB),
            ]),
            contactEmail='peloton-oncall-group@uber.com',
            executorConfig=self.get_executor_config(),
            container=container,
            constraints=set([host_limit]),
            requestedPorts=set(),
            mesosFetcherUris=set(),
            taskLinks={},
            metadata=set(),
        )

        job_config = JobConfiguration(
            key=self.job_key,
            owner=Identity(user=AURORA_USER),
            taskConfig=task_config,
            instanceCount=self.num_instances,
        )
        return job_config

    def get_current_job_config(self):
        """
        Return the current job config by querying the Aurora API
        """
        resp = self.client.getJobSummary(AURORA_ROLE)
        if resp.responseCode != ResponseCode.OK:
            raise Exception(combine_messages(resp))

        job_config = None
        for s in resp.result.jobSummaryResult.summaries:
            if s.job.key == self.job_key:
                job_config = s.job
                break

        if job_config:
            instances = self.get_instances()
            job_config.instanceCount = len(instances.get(Role.ALL, []))

        return job_config

    def get_instances(self):
        """
        Returns all instances grouped by role if exist by querying Aurora API
        """

        # Setup task query for task status of the Aurora job
        task_query = TaskQuery(
            role=AURORA_ROLE,
            environment=AURORA_ENVIRONMENT,
            jobName=self.name,
            statuses=ACTIVE_STATES,
        )

        resp = self.client.getTasksWithoutConfigs(task_query)
        if resp.responseCode != ResponseCode.OK:
            raise Exception(combine_messages(resp))

        instances = {}
        leader = True
        for t in resp.result.scheduleStatusResult.tasks:
            if t.status not in ACTIVE_STATES:
                # Ignore tasks that are not in active states
                continue

            # Temporarily hack to set leader/follower roles
            # TODO: query Peloton app endpoint to find role
            role = Role.LEADER if leader else Role.FOLLOWER
            if leader:
                leader = False

            inst = Instance(t, role)
            instances.setdefault(inst.role, []).append(inst)
            instances.setdefault(Role.ALL, []).append(inst)

        return instances

    def wait_for_running(self, role):
        """
        Wait for the app instances of a given role running
        """

        num_instances = {
            Role.LEADER: 1,
            Role.FOLLOWER: self.num_instances - 1,
            Role.ALL: self.num_instances,
        }[role]

        start_time = time()
        while time() < start_time + MAX_WAIT_TIME_SECONDS:
            instances = self.get_instances().get(role, [])
            all_running = True

            for i in instances:
                if i.state != ScheduleStatus.RUNNING:
                    all_running = False
                    break

            print 'Wait for %s %s instances running: %d / %d' % (
                self.name, role.name, all_running, len(instances))

            if all_running and len(instances) == num_instances:
                return True

            sleep(WAIT_INTERVAL_SECONDS)

        return False

    def update_instances(self, instances, job_config):
        """
        Update the task config of the given app instances
        """

        instance_ids = [i.instance_id for i in instances]

        req = JobUpdateRequest(
            taskConfig=job_config.taskConfig,
            instanceCount=self.num_instances,
            settings=JobUpdateSettings(
                updateGroupSize=1,
            ),
        )
        if instance_ids:
            req.settings.updateOnlyTheseInstances = set(
                Range(i, i) for i in instance_ids
            )

        resp = self.client.startJobUpdate(
            req,
            'Update %s instances for %s' % (len(instances), self.name)
        )
        if resp.responseCode == ResponseCode.INVALID_REQUEST:
            if resp.result is None:
                raise Exception(combine_messages(resp))

            update_key = resp.result.startJobUpdateResult.key
            update_summary = resp.result.startJobUpdateResult.updateSummary
            status = update_summary.state.status
            if status == JobUpdateStatus.ROLLING_FORWARD:
                # Abort the current update
                print 'Aborting the update for %s (id=%s)' % (
                    self.name, update_key.id)
                self.client.abortJobUpdate(
                    update_key, 'Abort by a new deploy session')
                self.wait_for_update_done(update_key)

                # Restart the job update
                resp = self.client.startJobUpdate(
                    req,
                    'Update %s instances for %s' % (len(instances), self.name)
                )
            else:
                raise Exception(
                    'Invalid Request for job update (status=%s)' % (
                        status, JobUpdateStatus._VALUES_TO_NAMES[status])
                )

        if resp.responseCode != ResponseCode.OK:
            raise Exception(combine_messages(resp))

        if resp.result is None:
            # No change for the job update
            print resp.details[0].message
            return True

        update_key = resp.result.startJobUpdateResult.key
        return self.wait_for_update_done(update_key, instance_ids)

    def wait_for_update_done(self, update_key, instance_ids=[]):
        """
        Wait for the job update to finish
        """

        query = JobUpdateQuery(
            role=AURORA_ROLE,
            key=update_key,
            jobKey=self.job_key
        )

        start_time = time()
        while time() < start_time + MAX_WAIT_TIME_SECONDS:
            resp = self.client.getJobUpdateSummaries(query)
            if resp.responseCode != ResponseCode.OK:
                print combine_messages(resp)
                sleep(WAIT_INTERVAL_SECONDS)
                continue

            result = resp.result.getJobUpdateSummariesResult

            if len(result.updateSummaries) != 1:
                raise Exception(
                    'Got multiple update summaries: %s' %
                    str(result.updateSummaries)
                )

            if result.updateSummaries[0].key != update_key:
                raise Exception(
                    'Mismatch update key, expect %s, received %s' %
                    (update_key, result.updateSummaries[0].key)
                )

            status = result.updateSummaries[0].state.status
            print 'Updating %s instances %s (status=%s)' % (
                self.name, instance_ids,
                JobUpdateStatus._VALUES_TO_NAMES[status])

            if status == JobUpdateStatus.ROLLED_FORWARD:
                return True
            elif status in [JobUpdateStatus.ROLLED_BACK,
                            JobUpdateStatus.ABORTED,
                            JobUpdateStatus.ERROR,
                            JobUpdateStatus.FAILED]:
                return False
            else:
                # Wait for 5 seconds
                sleep(WAIT_INTERVAL_SECONDS)

        return False

    def update_or_create_job(self, callback):
        """
        Update the current job for a Peloton app. Create a new job if the job
        does not exist yet.
        """

        # TODO: find the leader/follower role of each app instance

        if self.current_job_config is None:
            # Create the new Job in Aurora and check the response code
            print 'Creating new job for %s with %s instances' % (
                self.name, self.num_instances)

            resp = self.client.createJob(self.desired_job_config)
            if resp.responseCode != ResponseCode.OK:
                raise Exception(combine_messages(resp))

            # Wait for all instances are running
            retval = self.wait_for_running(Role.ALL)

            if retval:
                callback(self)

            return retval

        # Get current leader and follower instances
        cur_instances = self.get_instances()

        # First updade all existing instances, followers first then leader
        for role in [Role.FOLLOWER, Role.LEADER]:
            instances = cur_instances.get(role, [])

            if role == Role.LEADER and len(instances) > 1:
                raise Exception(
                    'Found %d leaders for %s' % (len(instances), self.name)
                )

            if len(instances) == 0:
                print 'No %s %s instances to update' % (self.name, role.name)
                continue

            print 'Start updating %d %s %s instances' % (
                len(instances), self.name, role.name)

            retval = self.update_instances(instances, self.desired_job_config)

            print 'Finish updating %d %s %s instances -- %s' % (
                len(instances), self.name, role.name,
                'SUCCEED' if retval else 'FAILED')

            if not retval or not callback(self):
                # Rollback the update by the caller
                return False

        # Then add any missing instances if needed
        cur_total = len(cur_instances.get(role.ALL, []))
        new_total = self.num_instances > cur_total

        if new_total > 0:
            print 'Start adding %d new %s instances' % (new_total, self.name)
            retval = self.update_instances([], self.desired_job_config)
            print 'Finish adding %d new %s instances -- %s' % (
                new_total, self.name,
                'SUCCEED' if retval else 'FAILED')

            if not retval or not callback(self):
                # Rollback the update by the caller
                return False

        return True

    def rollback_job(self):
        """
        Rollback the job config of a Peloton app in case of failures
        """
        if self.current_job_config is None:
            # Nothing to do if the job doesn't exist before
            return

        self.update_instances([], self.current_job_config)
