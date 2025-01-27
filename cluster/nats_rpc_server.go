// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package cluster

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	nats "github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/topfreegames/pitaya/v2/co"
	"github.com/topfreegames/pitaya/v2/config"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/metrics"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/util"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

var ErrAlreadySubscribed = errors.New("already subscribed")

// NatsRPCServer struct
type NatsRPCServer struct {
	service                int
	connString             string
	connectionTimeout      time.Duration
	maxReconnectionRetries int
	server                 *Server
	conn                   *nats.Conn
	pushBufferSize         int
	messagesBufferSize     int
	stopChan               chan bool
	subChan                chan *nats.Msg // subChan is the channel used by the server to receive network messages addressed to itself
	bindingsChan           chan *nats.Msg // bindingsChan receives notify from other servers on every user bind to session
	unhandledReqCh         chan *protos.Request
	responses              []*protos.Response
	requests               []*protos.Request
	userPushCh             chan *protos.Push
	userKickCh             chan *protos.KickMsg
	sub                    *nats.Subscription
	broadcastSubs          []*nats.Subscription          // 广播订阅
	publishSubs            map[string]*nats.Subscription // publish订阅:topic->sub
	preparePubSubTopics    map[string]string             // publish预备订阅:topic->group
	dropped                int
	pitayaServer           protos.PitayaServer
	metricsReporters       []metrics.Reporter
	sessionPool            session.SessionPool
	appDieChan             chan bool
	reqTimeout             time.Duration
}

// NewNatsRPCServer ctor
func NewNatsRPCServer(
	config config.NatsRPCServerConfig,
	server *Server,
	metricsReporters []metrics.Reporter,
	appDieChan chan bool,
	sessionPool session.SessionPool,
) (*NatsRPCServer, error) {
	ns := &NatsRPCServer{
		server:              server,
		stopChan:            make(chan bool),
		unhandledReqCh:      make(chan *protos.Request),
		dropped:             0,
		metricsReporters:    metricsReporters,
		appDieChan:          appDieChan,
		connectionTimeout:   nats.DefaultTimeout,
		sessionPool:         sessionPool,
		broadcastSubs:       make([]*nats.Subscription, 0),
		publishSubs:         map[string]*nats.Subscription{},
		preparePubSubTopics: map[string]string{},
	}
	if err := ns.configure(config); err != nil {
		return nil, err
	}

	return ns, nil
}

func (ns *NatsRPCServer) configure(config config.NatsRPCServerConfig) error {
	ns.service = config.Services
	ns.connString = config.Connect
	if ns.connString == "" {
		return constants.ErrNoNatsConnectionString
	}
	ns.connectionTimeout = config.ConnectionTimeout
	ns.maxReconnectionRetries = config.MaxReconnectionRetries
	ns.messagesBufferSize = config.Buffer.Messages
	if ns.messagesBufferSize == 0 {
		return constants.ErrNatsMessagesBufferSizeZero
	}
	ns.pushBufferSize = config.Buffer.Push
	if ns.pushBufferSize == 0 {
		return constants.ErrNatsPushBufferSizeZero
	}
	ns.subChan = make(chan *nats.Msg, ns.messagesBufferSize)
	ns.bindingsChan = make(chan *nats.Msg, ns.messagesBufferSize)
	// the reason this channel is buffered is that we can achieve more performance by not
	// blocking producers on a massive push
	ns.userPushCh = make(chan *protos.Push, ns.pushBufferSize)
	ns.userKickCh = make(chan *protos.KickMsg, ns.messagesBufferSize)
	ns.responses = make([]*protos.Response, ns.service)
	ns.requests = make([]*protos.Request, ns.service)
	ns.reqTimeout = config.RequestTimeout
	return nil
}

// GetBindingsChannel gets the channel that will receive all bindings
func (ns *NatsRPCServer) GetBindingsChannel() chan *nats.Msg {
	return ns.bindingsChan
}

// GetUserMessagesTopic get the topic for user
func GetUserMessagesTopic(uid string, svType string) string {
	return fmt.Sprintf("pitaya/%s/user/%s/push", svType, uid)
}

// GetUserKickTopic get the topic for kicking an user
func GetUserKickTopic(uid string, svType string) string {
	return fmt.Sprintf("pitaya/%s/user/%s/kick", svType, uid)
}

// GetBindBroadcastTopic gets the topic on which bind events will be broadcasted
func GetBindBroadcastTopic(svType string) string {
	return fmt.Sprintf("pitaya/%s/bindings", svType)
}

func GetForkTopic(svrType string) string {
	return fmt.Sprintf("pitaya.fork.%s", svrType)
}

const PublishServiceName = "publish"

func GetPublishTopic(topic string) string {
	return fmt.Sprintf("pitaya.%s.%s", PublishServiceName, topic)
}

// onSessionBind should be called on each session bind
func (ns *NatsRPCServer) onSessionBind(ctx context.Context, s session.Session, callback map[string]string) error {
	if ns.server.Frontend {
		subu, err := ns.subscribeToUserMessages(s.UID(), ns.server.Type)
		if err != nil {
			return err
		}
		subk, err := ns.subscribeToUserKickChannel(s.UID(), ns.server.Type)
		if err != nil {
			return err
		}
		s.SetSubscriptions([]*nats.Subscription{subu, subk})
	}
	return nil
}

// SetPitayaServer sets the pitaya server
func (ns *NatsRPCServer) SetPitayaServer(ps protos.PitayaServer) {
	ns.pitayaServer = ps
}

func (ns *NatsRPCServer) subscribeToBindingsChannel() error {
	_, err := ns.conn.ChanSubscribe(GetBindBroadcastTopic(ns.server.Type), ns.bindingsChan)
	return err
}

func (ns *NatsRPCServer) subscribeToUserKickChannel(uid string, svType string) (*nats.Subscription, error) {
	sub, err := ns.conn.Subscribe(GetUserKickTopic(uid, svType), func(msg *nats.Msg) {
		kick := &protos.KickMsg{}
		err := proto.Unmarshal(msg.Data, kick)
		if err != nil {
			logger.Zap.Error("error unrmarshalling push", zap.Error(err))
		}
		ns.userKickCh <- kick
	})
	return sub, err
}

func (ns *NatsRPCServer) subscribeToUserMessages(uid string, svType string) (*nats.Subscription, error) {
	sub, err := ns.conn.Subscribe(GetUserMessagesTopic(uid, svType), func(msg *nats.Msg) {
		push := &protos.Push{}
		err := proto.Unmarshal(msg.Data, push)
		if err != nil {
			logger.Zap.Error("error unmarshalling push", zap.Error(err))
		}
		logger.Zap.Debug("receive user's push", zap.String("uid", uid), zap.Int("remain", len(ns.userPushCh)))
		ns.userPushCh <- push
	})
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (ns *NatsRPCServer) handleMessages() {
	defer (func() {
		ns.conn.Drain()
		close(ns.unhandledReqCh)
		close(ns.subChan)
		close(ns.bindingsChan)
	})()
	maxPending := float64(0)
	for {
		select {
		case msg := <-ns.subChan:
			ns.reportMetrics()
			dropped, err := ns.sub.Dropped()
			if err != nil {
				logger.Zap.Error("error getting number of dropped messages", zap.Error(err))
			}
			// 添加广播订阅的消息丢失日志和统计
			for _, sub := range ns.broadcastSubs {
				tmpDropped, err := sub.Dropped()
				if err != nil {
					logger.Zap.Error("error getting number of dropped messages", zap.Error(err))
				}
				dropped += tmpDropped
			}
			for _, sub := range ns.publishSubs {
				tmpDropped, err := sub.Dropped()
				if err != nil {
					logger.Zap.Error("error getting number of dropped messages", zap.Error(err))
				}
				dropped += tmpDropped
			}
			if dropped > ns.dropped {
				logger.Log.Warnf("[rpc server] some messages were dropped! numDropped: %d", dropped)
				ns.dropped = dropped
			}
			subsChanLen := float64(len(ns.subChan))
			maxPending = math.Max(float64(maxPending), subsChanLen)
			logger.Log.Debugf("subs channel size: %f, max: %f, dropped: %d", subsChanLen, maxPending, dropped)
			req := &protos.Request{}
			// TODO: Add tracing here to report delay to start processing message in spans
			err = proto.Unmarshal(msg.Data, req)
			if err != nil {
				// should answer rpc with an error
				logger.Zap.Error("error unmarshalling rpc message", zap.Error(err))
				continue
			}
			req.Msg.Reply = msg.Reply
			ns.unhandledReqCh <- req
		case <-ns.stopChan:
			return
		}
	}
}

// GetUnhandledRequestsChannel gets the unhandled requests channel from nats rpc server
func (ns *NatsRPCServer) GetUnhandledRequestsChannel() chan *protos.Request {
	return ns.unhandledReqCh
}

func (ns *NatsRPCServer) getUserPushChannel() chan *protos.Push {
	return ns.userPushCh
}

func (ns *NatsRPCServer) getUserKickChannel() chan *protos.KickMsg {
	return ns.userKickCh
}

func (ns *NatsRPCServer) marshalResponse(res *protos.Response) ([]byte, error) {
	p, err := proto.Marshal(res)
	if err != nil {

		res := &protos.Response{
			Status: &apierrors.FromError(err).Status,
		}
		p, _ = proto.Marshal(res)
	}

	if err == nil && res.Status != nil {
		err = apierrors.FromStatus(res.Status)
	}
	return p, err
}

func (ns *NatsRPCServer) processMessages(threadID int) {
	for ns.requests[threadID] = range ns.GetUnhandledRequestsChannel() {
		req := ns.requests[threadID] // 拷贝一个给线程避免闭包问题
		logger.Log.Debugf("(%d) processing message %v", threadID, req.GetMsg().GetId())
		uid := ""
		if req.Session != nil {
			uid = req.Session.Uid
		}
		ctx, err := util.GetContextFromRequest(ns.requests[threadID], ns.server.ID, uid)
		if err != nil {
			ns.responses[threadID] = &protos.Response{
				Status: &apierrors.FromError(err).Status,
			}
			if ns.requests[threadID].GetMsg().Type != protos.MsgType_MsgNotify {
				p, err := ns.marshalResponse(ns.responses[threadID])
				err = ns.conn.Publish(ns.requests[threadID].GetMsg().GetReply(), p)
				if err != nil {
					logger.Zap.Error("error sending message response")
				}
			}
			continue
		}
		logg := util.GetLoggerFromCtx(ctx)
		logg.Debug("rpcsv processing msg")
		GoWithRequest(ctx, req, func(ctx context.Context) {
			resp, err := ns.pitayaServer.Call(ctx, req)
			if err != nil {
				// pitayaServer.Call已有打印error,这里不再重复
				logg.Info("rpc error calling pitayaServer", zap.String("cause", err.Error()))
			}
			if req.GetMsg().Type != protos.MsgType_MsgNotify {
				p, err := ns.marshalResponse(resp)
				err = ns.conn.Publish(req.GetMsg().GetReply(), p)
				if err != nil {
					logg.Error("error sending message response")
				}
			}
		})
	}
}

func (ns *NatsRPCServer) processSessionBindings() {
	for bind := range ns.bindingsChan {
		b := &protos.BindMsg{}
		err := proto.Unmarshal(bind.Data, b)
		if err != nil {
			logger.Zap.Error("error processing binding msg", zap.Error(err))
			continue
		}
		ns.pitayaServer.SessionBindRemote(context.Background(), b)
	}
}

func (ns *NatsRPCServer) processPushes() {
	for push := range ns.getUserPushChannel() {
		_, err := ns.pitayaServer.PushToUser(context.Background(), push)
		if err != nil {
			logger.Zap.Error("error sending push to user", zap.Error(err))
		}
	}
}

func (ns *NatsRPCServer) processKick() {
	for kick := range ns.getUserKickChannel() {
		logger.Log.Debugf("Sending kick to user %s: %v", kick.GetUserId())
		_, err := ns.pitayaServer.KickUser(context.Background(), kick)
		if err != nil {
			logger.Zap.Error("error sending kick to user", zap.Error(err))
		}
	}
}

// Init inits nats rpc server
func (ns *NatsRPCServer) Init() error {
	// TODO should we have concurrency here? it feels like we should
	co.Go(func() { ns.handleMessages() })

	logger.Log.Debugf("connecting to nats (server) with timeout of %s", ns.connectionTimeout)
	conn, err := setupNatsConn(
		ns.connString,
		ns.appDieChan,
		nats.MaxReconnects(ns.maxReconnectionRetries),
		nats.Timeout(ns.connectionTimeout),
	)
	if err != nil {
		return err
	}
	ns.conn = conn
	if ns.sub, err = ns.subscribe(getChannel(ns.server.Type, ns.server.ID), false); err != nil {
		return err
	}
	// fork订阅
	var bcstSub *nats.Subscription
	topic := GetForkTopic(ns.server.Type)
	if bcstSub, err = ns.subscribe(topic, false); err != nil {
		return err
	}
	ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)
	// publish订阅
	for t, group := range ns.preparePubSubTopics {
		var sub *nats.Subscription
		if group == "" {
			sub, err = ns.conn.ChanSubscribe(t, ns.subChan)
		} else {
			sub, err = ns.conn.ChanQueueSubscribe(t, group, ns.subChan)
		}
		if err != nil {
			return errors.WithStack(err)
		}
		ns.publishSubs[t] = sub
	}
	// topic = GetForkTopic("", false)
	// queue = NeedQueueSubscribe(ns.server, true, false)
	// if bcstSub, err = ns.subscribe(topic, queue); err != nil {
	//	return err
	// }
	// ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)
	// topic = GetForkTopic(ns.server.Type)
	// if bcstSub, err = ns.subscribe(topic, false); err != nil {
	//	return err
	// }
	// ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)
	// topic = GetForkTopic("", true)
	// queue = NeedQueueSubscribe(ns.server, true, true)
	// if bcstSub, err = ns.subscribe(topic, queue); err != nil {
	//	return err
	// }
	// ns.broadcastSubs = append(ns.broadcastSubs, bcstSub)

	err = ns.subscribeToBindingsChannel()
	if err != nil {
		return err
	}
	// this handles remote messages
	// for i := 0; i < ns.service; i++ {
	// 	threadID := i // 避免闭包值拷贝问题
	// 	co.Go(func() { ns.processMessages(threadID) })
	// }
	// 以上改为单线程 使投递过程保序 投递后再session多线程处理
	co.Go(func() { ns.processMessages(0) })

	ns.sessionPool.OnSessionBind(ns.onSessionBind)

	// this should be so fast that we shoudn't need concurrency
	co.Go(func() { ns.processPushes() })
	co.Go(func() { ns.processSessionBindings() })
	co.Go(func() { ns.processKick() })

	return nil
}

// AfterInit runs after initialization
func (ns *NatsRPCServer) AfterInit() {}

// BeforeShutdown runs before shutdown
func (ns *NatsRPCServer) BeforeShutdown() {}

// Shutdown stops nats rpc server
func (ns *NatsRPCServer) Shutdown() error {
	close(ns.stopChan)
	return nil
}

func (ns *NatsRPCServer) subscribe(topic string, queue bool) (*nats.Subscription, error) {
	if queue {
		return ns.conn.ChanQueueSubscribe(topic, ns.server.Type, ns.subChan)
	}
	return ns.conn.ChanSubscribe(topic, ns.subChan)
}

func (ns *NatsRPCServer) stop() {
}

func (ns *NatsRPCServer) Subscribe(topic string, groups ...string) error {
	topic = GetPublishTopic(topic)
	// TODO 线程不安全,若后续真有动态订阅需求再优化
	sub, ok := ns.publishSubs[topic]
	if ok {
		logger.Zap.Warn("", zap.String("topic", topic), zap.Error(ErrAlreadySubscribed))
		return nil
	}
	_, ok = ns.preparePubSubTopics[topic]
	if ok {
		logger.Zap.Warn("", zap.String("topic", topic), zap.Error(ErrAlreadySubscribed))
		return nil
	}
	var err error
	group := ""
	if len(groups) > 0 {
		group = groups[0]
	}
	// 若还未连接则加入预备
	if ns.conn == nil {
		ns.preparePubSubTopics[topic] = group
		return nil
	}
	// 已连接直接订阅
	if group == "" {
		sub, err = ns.conn.ChanSubscribe(topic, ns.subChan)
	} else {
		sub, err = ns.conn.ChanQueueSubscribe(topic, group, ns.subChan)
	}
	if err != nil {
		return errors.WithStack(err)
	}
	ns.publishSubs[topic] = sub
	return nil
}

func (ns *NatsRPCServer) reportMetrics() {
	if ns.metricsReporters != nil {
		for _, mr := range ns.metricsReporters {
			if err := mr.ReportGauge(metrics.DroppedMessages, map[string]string{}, float64(ns.dropped)); err != nil {
				logger.Zap.Warn("failed to report dropped message", zap.Error(err))
			}

			// subchan
			subChanCapacity := ns.messagesBufferSize - len(ns.subChan)
			if subChanCapacity == 0 {
				logger.Log.Warn("subChan is at maximum capacity")
			}
			if err := mr.ReportGauge(metrics.ChannelCapacity, map[string]string{"channel": "rpc_server_subchan"}, float64(subChanCapacity)); err != nil {
				logger.Zap.Warn("failed to report subChan queue capacity", zap.Error(err))
			}

			// bindingschan
			bindingsChanCapacity := ns.messagesBufferSize - len(ns.bindingsChan)
			if bindingsChanCapacity == 0 {
				logger.Log.Warn("bindingsChan is at maximum capacity")
			}
			if err := mr.ReportGauge(metrics.ChannelCapacity, map[string]string{"channel": "rpc_server_bindingschan"}, float64(bindingsChanCapacity)); err != nil {
				logger.Zap.Warn("failed to report bindingsChan capacity", zap.Error(err))
			}

			// userpushch
			userPushChanCapacity := ns.pushBufferSize - len(ns.userPushCh)
			if userPushChanCapacity == 0 {
				logger.Log.Warn("userPushChan is at maximum capacity")
			}
			if err := mr.ReportGauge(metrics.ChannelCapacity, map[string]string{"channel": "rpc_server_userpushchan"}, float64(userPushChanCapacity)); err != nil {
				logger.Zap.Warn("failed to report userPushCh capacity", zap.Error(err))
			}
		}
	}
}
