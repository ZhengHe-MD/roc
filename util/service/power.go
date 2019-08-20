// Copyright 2014 The roc Author. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rocserv

import (
	"context"
	"fmt"
	"github.com/ZhengHe-MD/go-stdlib/nethttp"
	"github.com/opentracing/opentracing-go"
	"github.com/uber/jaeger-client-go"
	"net"
	"net/http"
	"time"

	"github.com/julienschmidt/httprouter"

	"git.apache.org/thrift.git/lib/go/thrift"

	"github.com/shawnfeng/sutil/slog"
	"github.com/shawnfeng/sutil/snetutil"
)

func powerHttp(addr string, router *httprouter.Router) (string, error) {
	fun := "powerHttp -->"

	paddr, err := snetutil.GetListenAddr(addr)
	if err != nil {
		return "", err
	}

	slog.Infof("%s config addr[%s]", fun, paddr)

	tcpAddr, err := net.ResolveTCPAddr("tcp", paddr)
	if err != nil {
		return "", err
	}

	netListen, err := net.Listen(tcpAddr.Network(), tcpAddr.String())
	if err != nil {
		return "", err
	}

	laddr, err := snetutil.GetServAddr(netListen.Addr())
	if err != nil {
		netListen.Close()
		return "", err
	}

	slog.Infof("%s listen addr[%s]", fun, laddr)

	// tracing
	mw := nethttp.Middleware(
		opentracing.GlobalTracer(),
		router,
		nethttp.OperationNameFunc(func(r *http.Request) string {
			return "HTTP " + r.Method + ": " + r.URL.Path
		}),
		nethttp.MWSpanOnStart(func(ctx context.Context, span opentracing.Span, r *http.Request) context.Context {
			return context.WithValue(ctx, "SpanStartTime", time.Now())
		}),
		nethttp.MWSpanOnFinish(func(ctx context.Context, span opentracing.Span, r *http.Request) context.Context {
			if sctx, ok := span.Context().(jaeger.SpanContext); ok {
				if !sctx.IsSampled() {
					v := ctx.Value("SpanStartTime")
					if st, ok := v.(time.Time); ok {
						et := time.Now()
						slog.Infof("tid:%v sid:%v pid:%v st:%v ft:%v",
							sctx.TraceID(), sctx.SpanID(), sctx.ParentID(), st.Format(time.RFC3339), et.Format(time.RFC3339))
					}
				}
			}
			return ctx
		}))

	go func() {
		err := http.Serve(netListen, mw)
		if err != nil {
			slog.Panicf("%s laddr[%s]", fun, laddr)
		}
	}()

	return laddr, nil
}

func powerThrift(addr string, processor thrift.TProcessor) (string, error) {
	fun := "powerThrift -->"

	paddr, err := snetutil.GetListenAddr(addr)
	if err != nil {
		return "", err
	}

	slog.Infof("%s config addr[%s]", fun, paddr)

	transportFactory := thrift.NewTFramedTransportFactory(thrift.NewTTransportFactory())
	protocolFactory := thrift.NewTBinaryProtocolFactoryDefault()
	//protocolFactory := thrift.NewTCompactProtocolFactory()

	serverTransport, err := thrift.NewTServerSocket(paddr)
	if err != nil {
		return "", err
	}

	server := thrift.NewTSimpleServer4(processor, serverTransport, transportFactory, protocolFactory)

	// Listen后就可以拿到端口了
	//err = server.Listen()
	err = serverTransport.Listen()
	if err != nil {
		return "", err
	}

	laddr, err := snetutil.GetServAddr(serverTransport.Addr())
	if err != nil {
		return "", err
	}

	slog.Infof("%s listen addr[%s]", fun, laddr)

	go func() {
		err := server.Serve()
		if err != nil {
			slog.Panicf("%s laddr[%s]", fun, laddr)
		}
	}()

	return laddr, nil

}

//启动grpc ，并返回端口信息
func powerGrpc(addr string, server *GrpcServer) (string, error) {
	fun := "powerGrpc -->"
	paddr, err := snetutil.GetListenAddr(addr)
	if err != nil {
		return "", err
	}
	slog.Infof("%s config addr[%s]", fun, paddr)
	lis, err := net.Listen("tcp", paddr)
	if err != nil {
		return "", fmt.Errorf("grpc tcp Listen err:%v", err)
	}
	laddr, err := snetutil.GetServAddr(lis.Addr())
	if err != nil {
		return "", fmt.Errorf(" GetServAddr err:%v", err)
	}
	slog.Infof("%s listen grpc addr[%s]", fun, laddr)
	go func() {
		if err := server.Server.Serve(lis); err != nil {
			slog.Panicf("%s grpc laddr[%s]", fun, laddr)
		}
	}()
	return laddr, nil
}
