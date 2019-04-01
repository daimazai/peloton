import pytest
import time

from tests.integration.aurorabridge_test.client import api
from tests.integration.aurorabridge_test.util import (
    get_job_update_request,
    get_update_status,
    start_job_update,
    wait_for_update_status,
)

pytestmark = [pytest.mark.default,
              pytest.mark.aurorabridge,
              pytest.mark.random_order(disabled=True)]


def test__start_job_update_with_pulse(client):
    req = get_job_update_request('test_dc_labrat_pulsed.yaml')
    res = client.start_job_update(req, 'start pulsed job update test/dc/labrat')
    assert get_update_status(client, res.key) == \
        api.JobUpdateStatus.ROLL_FORWARD_AWAITING_PULSE

    client.pulse_job_update(res.key)
    wait_for_update_status(
        client,
        res.key,
        {
            api.JobUpdateStatus.ROLL_FORWARD_AWAITING_PULSE,
            api.JobUpdateStatus.ROLLING_FORWARD,
        },
        api.JobUpdateStatus.ROLLED_FORWARD)


def test__start_job_update_revocable_job(client):
    """
    Given 12 non-revocable cpus, and 12 revocable cpus
    Create a non-revocable of 3 instance, with 3 CPU per instance
    Create a revocable job of 2 instance, with 2 CPU per instance
    """
    non_revocable_job = start_job_update(
        client,
        'test_dc_labrat_cpus_large.yaml',
        'start job update test/dc/labrat_large')

    revocable_job = start_job_update(
        client,
        'test_dc_labrat_revocable.yaml',
        'start job update test/dc/labrat_revocable')

    # Add some wait time for lucene index to build
    time.sleep(10)

    # validate 3 non-revocable tasks are running
    res = client.get_tasks_without_configs(api.TaskQuery(
        jobKeys={non_revocable_job},
        statuses={api.ScheduleStatus.RUNNING}
    ))
    assert len(res.tasks) == 3

    # validate 2 revocable tasks are running
    res = client.get_tasks_without_configs(api.TaskQuery(
        jobKeys={revocable_job},
        statuses={api.ScheduleStatus.RUNNING}
    ))
    assert len(res.tasks) == 2


def test__failed_update(client):
    """
    update failed
    """
    res = client.start_job_update(
        get_job_update_request('test_dc_labrat_bad_config.yaml'),
        'rollout bad config')

    wait_for_update_status(
        client,
        res.key,
        {api.JobUpdateStatus.ROLLING_FORWARD},
        api.JobUpdateStatus.FAILED)


def test__start_job_update_with_msg(client):
    update_msg = 'update msg 1'
    job_key = start_job_update(client, 'test_dc_labrat.yaml', update_msg)

    res = client.get_job_update_details(None, api.JobUpdateQuery(jobKey=job_key))

    assert len(res.detailsList) == 1

    # verify events are sorted ascending
    assert len(res.detailsList[0].updateEvents) > 0
    update_events_ts = [e.timestampMs for e in res.detailsList[0].updateEvents]
    assert update_events_ts == sorted(update_events_ts)
    assert len(res.detailsList[0].instanceEvents) > 0
    instance_events_ts = [e.timestampMs for e in res.detailsList[0].instanceEvents]
    assert instance_events_ts == sorted(instance_events_ts)

    assert res.detailsList[0].updateEvents[0].status == api.JobUpdateStatus.ROLLING_FORWARD
    assert res.detailsList[0].updateEvents[0].message == update_msg
    assert res.detailsList[0].updateEvents[-1].status == api.JobUpdateStatus.ROLLED_FORWARD
