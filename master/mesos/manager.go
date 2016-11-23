package mesos

import (
	"code.uber.internal/go-common.git/x/log"
	"code.uber.internal/infra/peloton/yarpc/encoding/mpb"
	"go.uber.org/yarpc"

	"code.uber.internal/infra/peloton/storage"
	sched "mesos/v1/scheduler"
)

func InitManager(d yarpc.Dispatcher, mesosConfig *Config, store storage.FrameworkInfoStore) {
	m := mesosManager{
		store:       store,
		mesosConfig: mesosConfig,
	}

	procedures := map[sched.Event_Type]interface{}{
		sched.Event_SUBSCRIBED: m.Subscribed,
		sched.Event_MESSAGE:    m.Message,
		sched.Event_FAILURE:    m.Failure,
		sched.Event_ERROR:      m.Error,
		sched.Event_HEARTBEAT:  m.Heartbeat,
		sched.Event_UNKNOWN:    m.Unknown,
	}
	for typ, hdl := range procedures {
		name := typ.String()
		mpb.Register(d, ServiceName, mpb.Procedure(name, hdl))
	}
}

type mesosManager struct {
	store       storage.FrameworkInfoStore
	mesosConfig *Config
}

func (m *mesosManager) Subscribed(
	reqMeta yarpc.ReqMeta, body *sched.Event) error {

	subscribed := body.GetSubscribed()
	log.WithField("params", subscribed).Debug("mesosManager: subscribed called")
	frameworkId := subscribed.GetFrameworkId().GetValue()
	err := m.store.SetMesosFrameworkId(m.mesosConfig.Framework.Name, frameworkId)
	if err != nil {
		log.Errorf("failed to SetMesosFrameworkId %v %v, err=%v", m.mesosConfig.Framework.Name, frameworkId, err)
	}
	return err
}

func (m *mesosManager) Message(
	reqMeta yarpc.ReqMeta, body *sched.Event) error {

	msg := body.GetMessage()
	log.WithField("params", msg).Debug("mesosManager: message called")
	return nil
}

func (m *mesosManager) Failure(
	reqMeta yarpc.ReqMeta, body *sched.Event) error {

	failure := body.GetFailure()
	log.WithField("params", failure).Debug("mesosManager: failure called")
	return nil
}

func (m *mesosManager) Error(
	reqMeta yarpc.ReqMeta, body *sched.Event) error {

	err := body.GetError()
	log.WithField("params", err).Debug("mesosManager: error called")
	return nil
}

func (m *mesosManager) Heartbeat(
	reqMeta yarpc.ReqMeta, body *sched.Event) error {

	log.Debugf("mesosManager: heartbeat called")
	return nil
}

func (m *mesosManager) Unknown(
	reqMeta yarpc.ReqMeta, body *sched.Event) error {

	log.Infof("mesosManager: unknown event called")
	return nil
}
