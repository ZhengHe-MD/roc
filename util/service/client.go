// Copyright 2014 The roc Author. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rocserv

import (
	"fmt"
	"git.apache.org/thrift.git/lib/go/thrift"
	"github.com/shawnfeng/roc/util/service/sla"
	"github.com/shawnfeng/sutil/slog"
	"github.com/shawnfeng/sutil/stime"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type ClientLookup interface {
	GetServAddr(processor, key string) *ServInfo
	GetServAddrWithServid(servid int, processor, key string) *ServInfo
	GetAllServAddr(processor string) []*ServInfo
	ServKey() string
	ServPath() string
	GetBreakerServConf() string
	GetBreakerGlobalConf() string
}

func NewClientLookup(etcdaddrs []string, baseLoc string, servlocation string) (*ClientEtcdV2, error) {
	return NewClientEtcdV2(configEtcd{etcdaddrs, baseLoc}, servlocation, THRIFT)
}

func NewGrpcClientLookup(etcdaddrs []string, baseLoc string, servlocation string) (*ClientEtcdV2, error) {
	return NewClientEtcdV2(configEtcd{etcdaddrs, baseLoc}, servlocation, GRPC)
}

type ClientWrapper struct {
	clientLookup ClientLookup
	processor    string
	breaker      *Breaker
	router       Router
}

func NewClientWrapper(cb ClientLookup, processor string) *ClientWrapper {
	return NewClientWrapperWithRouterType(cb, processor, 0)
}

func NewClientWrapperByConcurrentRouter(cb ClientLookup, processor string) *ClientWrapper {
	return NewClientWrapperWithRouterType(cb, processor, 1)
}

func NewClientWrapperWithRouterType(cb ClientLookup, processor string, routerType int) *ClientWrapper {
	return &ClientWrapper{
		clientLookup: cb,
		processor:    processor,
		breaker:      NewBreaker(cb),
		router:       NewRouter(routerType, cb),
	}
}

func (m *ClientWrapper) Do(haskkey string, timeout time.Duration, run func(addr string, timeout time.Duration) error) error {
	fun := "ClientWrapper.Do -->"
	si := m.router.Route(m.processor, haskkey)
	if si == nil {
		return fmt.Errorf("%s not find service:%s processor:%s", fun, m.clientLookup.ServPath(), m.processor)
	}
	m.router.Pre(si)
	defer m.router.Post(si)

	call := func(addr string, timeout time.Duration) func() error {
		return func() error {
			return run(addr, timeout)
		}
	}(si.Addr, timeout)

	funcName := GetFunName(1)
	var err error
	st := stime.NewTimeStat()
	defer func() {
		collector(m.clientLookup.ServKey(), m.processor, st.Duration(), 0, si.Servid, funcName, err)
	}()
	err = m.breaker.Do(0, si.Servid, funcName, call, HTTP, nil)
	return err
}

var metricReqNameKeys = []string{rocserv.Name_space_palfish, rocserv.Name_server_req_total}
var metricDurationNameKeys = []string{rocserv.Name_space_palfish, rocserv.Name_server_duration_second}

func collector(servkey string, processor string, duration time.Duration, source int, servid int, funcName string, err interface{}) {
	durlabels := buildSerLabels(servkey, processor, source, servid, funcName)
	rocserv.DefaultMetrics.AddHistoramSampleCreateIfAbsent(metricDurationNameKeys, duration.Seconds(), durlabels, nil)
	var counterLabels []rocserv.Label
	if err == nil {
		counterLabels = buildSerReqLabels(servkey, processor, source, servid, funcName, rocserv.Status_succ)
	} else {
		counterLabels = buildSerReqLabels(servkey, processor, source, servid, funcName, rocserv.Status_fail)
	}
	rocserv.DefaultMetrics.IncrCounterCreateIfAbsent(metricReqNameKeys, 1.0, counterLabels)
}
func buildSerLabels(servkey string, processor string, source int, servid int, funcName string) []rocserv.Label {
	serverName := rocserv.SafePromethuesValue(servkey)
	sid := strconv.Itoa(servid)
	return []rocserv.Label{
		{Name: rocserv.Label_instance, Value: serverName + "_" + sid},
		{Name: rocserv.Label_servname, Value: serverName},
		{Name: rocserv.Label_servid, Value: sid},
		{Name: rocserv.Label_api, Value: funcName},
		{Name: rocserv.Label_source, Value: strconv.Itoa(source)},
		{Name: rocserv.Label_type, Value: processor},
	}
}
func buildSerReqLabels(servkey string, processor string, source int, servid int, funcName string, status int) []rocserv.Label {
	labels := buildSerLabels(servkey, processor, source, servid, funcName)
	labels = append(labels, rocserv.Label{
		Name: rocserv.Label_status, Value: strconv.Itoa(status),
	})
	return labels
}

type ClientThrift struct {
	clientLookup ClientLookup
	processor    string
	fnFactory    func(thrift.TTransport, thrift.TProtocolFactory) interface{}
	pool         *ClientPool
	breaker      *Breaker
	router       Router
}

type rpcClient interface {
	Close() error
	SetTimeout(timeout time.Duration) error
	GetServiceClient() interface{}
}

type rpcClient1 struct {
	tsock         *thrift.TSocket
	trans         thrift.TTransport
	serviceClient interface{}
}

func (m *rpcClient1) SetTimeout(timeout time.Duration) error {
	return m.tsock.SetTimeout(timeout)
}

func (m *rpcClient1) Close() error {
	return m.trans.Close()
}

func (m *rpcClient1) GetServiceClient() interface{} {
	return m.serviceClient
}

func NewClientThrift(cb ClientLookup, processor string, fn func(thrift.TTransport, thrift.TProtocolFactory) interface{}, poollen int) *ClientThrift {
	return NewClientThriftWithRouterType(cb, processor, fn, poollen, 0)
}

func NewClientThriftByConcurrentRouter(cb ClientLookup, processor string, fn func(thrift.TTransport, thrift.TProtocolFactory) interface{}, poollen int) *ClientThrift {
	return NewClientThriftWithRouterType(cb, processor, fn, poollen, 1)
}

func NewClientThriftWithRouterType(cb ClientLookup, processor string, fn func(thrift.TTransport, thrift.TProtocolFactory) interface{}, poollen, routerType int) *ClientThrift {

	ct := &ClientThrift{
		clientLookup: cb,
		processor:    processor,
		fnFactory:    fn,
		breaker:      NewBreaker(cb),
		router:       NewRouter(routerType, cb),
	}
	pool := NewClientPool(poollen, ct.newClient)
	ct.pool = pool

	// init trace
	InitJaeger(processor)

	return ct
}

func (m *ClientThrift) newClient(addr string) rpcClient {
	fun := "ClientThrift.newClient -->"

	transportFactory := thrift.NewTFramedTransportFactory(thrift.NewTTransportFactory())
	protocolFactory := thrift.NewTBinaryProtocolFactoryDefault()

	transport, err := thrift.NewTSocket(addr)
	if err != nil {
		slog.Errorf("%s NetTSocket addr:%s err:%s", fun, addr, err)
		return nil
	}
	useTransport := transportFactory.GetTransport(transport)

	if err := useTransport.Open(); err != nil {
		slog.Errorf("%s Open addr:%s err:%s", fun, addr, err)
		return nil
	}
	// 必须要close么？
	//useTransport.Close()

	slog.Infof("%s new client addr:%s", fun, addr)
	return &rpcClient1{
		tsock:         transport,
		trans:         useTransport,
		serviceClient: m.fnFactory(useTransport, protocolFactory),
	}
}

func (m *ClientThrift) route(key string) (*ServInfo, rpcClient) {
	s := m.router.Route(m.processor, key)
	if s == nil {
		return nil, nil
	}
	addr := s.Addr
	return s, m.pool.GrtClient(addr)
}

func (m *ClientThrift) Rpc(haskkey string, timeout time.Duration, fnrpc func(interface{}) error) error {
	//fun := "ClientThrift.Rpc-->"

	si, rc := m.route(haskkey)
	if rc == nil {
		return fmt.Errorf("not find thrift service:%s processor:%s", m.clientLookup.ServPath(), m.processor)
	}

	m.router.Pre(si)
	defer m.router.Post(si)

	call := func(si *ServInfo, rc rpcClient, timeout time.Duration, fnrpc func(interface{}) error) func() error {
		return func() error {
			return m.rpc(si, rc, timeout, fnrpc)
		}
	}(si, rc, timeout, fnrpc)

	funcName := GetFunName(2)
	var err error
	st := stime.NewTimeStat()
	defer func() {
		collector(m.clientLookup.ServKey(), m.processor, st.Duration(), 0, si.Servid, funcName, err)
	}()
	err = m.breaker.Do(0, si.Servid, funcName, call, THRIFT, nil)
	return err
}

func (m *ClientThrift) rpc(si *ServInfo, rc rpcClient, timeout time.Duration, fnrpc func(interface{}) error) error {
	fun := "ClientThrift.rpc -->"

	rc.SetTimeout(timeout)
	c := rc.GetServiceClient()

	err := fnrpc(c)
	if err == nil {
		m.pool.Put(si.Addr, rc)
	} else {
		slog.Warnf("%s close rpcclient s:%s", fun, si)
		rc.Close()
	}
	return err
}

func GetFunName(index int) string {
	funcName := ""
	pc, _, _, ok := runtime.Caller(index)
	if ok {
		funcName = runtime.FuncForPC(pc).Name()
		if index := strings.LastIndex(funcName, "."); index != -1 {
			if len(funcName) > index+1 {
				funcName = funcName[index+1:]
			}
		}
	}
	return funcName
}
