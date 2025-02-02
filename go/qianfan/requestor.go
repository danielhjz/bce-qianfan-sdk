// Copyright (c) 2024 Baidu, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package qianfan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/baidubce/bce-sdk-go/auth"
	bceHTTP "github.com/baidubce/bce-sdk-go/http"
	"github.com/baidubce/bce-sdk-go/util"
)

// 所有请求类型需实现的接口
//
// 定义了提供额外参数的接口
type RequestBody interface {
	SetExtra(m map[string]interface{})
	GetExtra() map[string]interface{}
}

// 请求体基类
//
// 实现了允许用户传递额外参数的方法
type BaseRequestBody struct {
	Extra map[string]interface{} `mapstructure:"-"`
}

// 设置额外参数
func (r *BaseRequestBody) SetExtra(m map[string]interface{}) {
	r.Extra = m
}

// 获取额外参数
func (r *BaseRequestBody) GetExtra() map[string]interface{} {
	return r.Extra
}

// 将请求设置为流式
func (r *BaseRequestBody) SetStream() {
	if r.Extra == nil {
		r.Extra = map[string]interface{}{}
	}
	r.Extra["stream"] = true
}

// 将请求体转换成 map，并将额外参数合并到 map 中
func convertToMap(body RequestBody) (map[string]interface{}, error) {
	m, err := dumpToMap(body)
	if err != nil {
		return nil, err
	}
	extra := body.GetExtra()
	for k, v := range extra {
		m[k] = v
	}
	return m, nil
}

// 请求类型，用于区分是模型的请求还是管控类请求
// 在 QfRequest.Type 处被使用
const (
	modelRequest   = "model"
	consoleRequest = "console"
)

// SDK 内部表示请求的类
type QfRequest struct {
	Type    string                 // 请求类型，用于区分是模型的请求 `modelRequest` 还是管控类请求 `consoleRequest`
	Method  string                 // HTTP 方法
	URL     string                 // 请求的完整地址
	Headers map[string]string      // HTTP 请求头
	Params  map[string]string      // HTTP 请求参数
	Body    map[string]interface{} // HTTP 请求体
}

// 创建一个用于模型类请求的 Request
func newModelRequest(method string, url string, body RequestBody) (*QfRequest, error) {
	return newRequest(modelRequest, method, url, body)
}

// 创建一个用于管控类请求的 Request
// 暂时注释避免 lint 报错
// func newConsoleRequest(method string, url string, body RequestBody) (*QfRequest, error) {
// 	return newRequest(ConsoleRequest, method, url, body)
// }

// 创建一个 Request，body 可以是任意实现了 RequestBody 接口的类型
func newRequest(requestType string, method string, url string, body RequestBody) (*QfRequest, error) {
	b, err := convertToMap(body)
	if err != nil {
		return nil, err
	}
	return newRequestFromMap(requestType, method, url, b)
}

// 创建一个 Request，body 是一个 map
func newRequestFromMap(requestType string, method string, url string, body map[string]interface{}) (*QfRequest, error) {
	return &QfRequest{
		Type:    requestType,
		Method:  method,
		URL:     url,
		Body:    body,
		Params:  map[string]string{},
		Headers: map[string]string{},
	}, nil
}

// 所有回复类型的基类
type baseResponse struct {
	Body        []byte
	RawResponse *http.Response
}

// 所有回复类型需实现的接口
type QfResponse interface {
	SetResponse(Body []byte, RawResponse *http.Response)
}

// 设置回复中通用参数的字段
func (r *baseResponse) SetResponse(Body []byte, RawResponse *http.Response) {
	r.Body = Body
	r.RawResponse = RawResponse
}

// 请求器，负责 SDK 中所有请求的发送，是所有对外暴露对象的基类
type Requestor struct {
	client  *http.Client
	Options *Options
}

// 创建一个 Requestor
func newRequestor(options *Options) *Requestor {
	return &Requestor{
		client:  &http.Client{},
		Options: options,
	}
}

// IAM 签名
func (r *Requestor) sign(request *QfRequest) error {
	bceRequest := &bceHTTP.Request{}
	bceRequest.SetMethod(request.Method)
	bceRequest.SetHeaders(request.Headers)
	bceRequest.SetParams(request.Params)
	u, err := url.Parse(request.URL)
	if err != nil {
		return err
	}
	bceRequest.SetProtocol(u.Scheme)

	bceRequest.SetHost(u.Hostname())
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else if u.Scheme == "https" {
			port = "443"
		} else {
			return fmt.Errorf("unrecognized scheme: %s", u.Scheme)
		}
	}
	porti, err := strconv.Atoi(port)
	if err != nil {
		return err
	}
	bceRequest.SetPort(porti)
	bceRequest.SetUri(u.RequestURI())

	credentials := &auth.BceCredentials{
		AccessKeyId:     GetConfig().AccessKey,
		SecretAccessKey: GetConfig().SecretKey,
	}
	now := util.NowUTCSeconds()
	bceRequest.SetHeader("Host", u.Hostname())
	bceRequest.SetHeader("x-bce-date", util.FormatISO8601Date(now))
	headersToSign := make(map[string]struct{})
	for k := range bceRequest.Headers() {
		headersToSign[strings.ToLower(k)] = struct{}{}
	}
	signer := auth.BceV1Signer{}
	signOptions := &auth.SignOptions{
		HeadersToSign: headersToSign,
		Timestamp:     now,
		ExpireSeconds: GetConfig().IAMSignExpirationSeconds,
	}
	signer.Sign(bceRequest, credentials, signOptions)

	request.Headers = bceRequest.Headers()
	return nil
}

// 对请求进行统一处理，并转换成 http.Request
func (r *Requestor) prepareRequest(request *QfRequest) (*http.Request, error) {
	// 设置溯源标识
	if request.Type == modelRequest {
		request.URL = GetConfig().BaseURL + request.URL
		request.Body["extra_parameters"] = map[string]string{
			"request_source": versionIndicator,
		}
	} else if request.Type == consoleRequest {
		request.URL = GetConfig().ConsoleBaseURL + request.URL
		request.Headers["request-source"] = versionIndicator
	}
	bodyBytes, err := json.Marshal(request.Body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(request.Method, request.URL, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, err
	}
	request.Headers["Content-Type"] = "application/json"
	// IAM 签名
	err = r.sign(request)
	if err != nil {
		return nil, err
	}
	for k, v := range request.Headers {
		req.Header.Set(k, v)
	}

	q := req.URL.Query()
	for k, v := range request.Params {
		q.Add(k, v)
	}
	req.URL.RawQuery = q.Encode()

	return req, nil
}

// 进行请求，返回原始的 baseResponse，并将结果解析至 resp
func (r *Requestor) request(request *QfRequest, response QfResponse) error {
	req, err := r.prepareRequest(request)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	response.SetResponse(body, resp)
	err = json.Unmarshal(body, response)
	if err != nil {
		return err
	}
	return nil
}

// 流的内部实现，用于接收流中的响应
type streamInternal struct {
	httpResponse *http.Response // 原始的 http.Response
	scanner      *bufio.Scanner // 读取流的 scanner
	IsEnd        bool           // 流是否已经结束
}

// 创建一个流
func newStreamInternal(httpResponse *http.Response) (*streamInternal, error) {
	return &streamInternal{
		httpResponse: httpResponse,
		scanner:      bufio.NewScanner(httpResponse.Body),
		IsEnd:        false,
	}, nil
}

// 关闭流
func (si *streamInternal) Close() {
	_ = si.httpResponse.Body.Close()
}

// 接受流中的响应，并将结果解析至 resp
func (si *streamInternal) Recv(resp QfResponse) error {
	var eventData []byte
	for len(eventData) == 0 {
		for {
			if !si.scanner.Scan() {
				si.IsEnd = true
				si.Close()
				return si.scanner.Err()
			}

			line := si.scanner.Bytes()
			if len(line) == 0 {
				break
			}
			var (
				// field []byte = line
				value []byte
			)
			if i := bytes.IndexRune(line, ':'); i != -1 {
				// field = line[:i]
				value = line[i+1:]
				if len(value) != 0 && value[0] == ' ' {
					value = value[1:]
				}
			}
			eventData = append(eventData, value...)
		}
	}
	response := baseResponse{
		Body:        eventData,
		RawResponse: si.httpResponse,
	}

	resp.SetResponse(response.Body, response.RawResponse)
	err := json.Unmarshal(response.Body, resp)
	if err != nil {
		si.IsEnd = true
		return err
	}
	return nil
}

// 发送请求，返回流对象
func (r *Requestor) requestStream(request *QfRequest) (*streamInternal, error) {
	req, err := r.prepareRequest(request)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	stream, err := newStreamInternal(resp)
	if err != nil {
		return nil, err
	}
	return stream, nil
}
