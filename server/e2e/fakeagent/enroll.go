// SPDX-License-Identifier: AGPL-3.0-or-later

package fakeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"
)

// EnrollError 是接入接口返回的错误（problem+json，enrollment.proto）。
type EnrollError struct {
	Status     int
	Code       string
	RetryAfter time.Duration
}

func (e *EnrollError) Error() string {
	return fmt.Sprintf("fakeagent: enrollment failed: %d %s", e.Status, e.Code)
}

// Enroll 以 POST {gatewayURL}/v1/enrollments 提交接入请求，换取节点 ID 与 PSK（NODE-02、NODE-18）。
// 请求体与响应体为 protobuf 编码，错误为 problem+json。
func Enroll(ctx context.Context, client *http.Client, gatewayURL string, req *nodev1.EnrollRequest) (*nodev1.EnrollResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(gatewayURL, "/")+"/v1/enrollments", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	hr.Header.Set("Accept", "application/x-protobuf, application/problem+json")
	resp, err := client.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("fakeagent: enrollment: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("fakeagent: enrollment: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		e := &EnrollError{Status: resp.StatusCode}
		var p struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(b, &p) == nil {
			e.Code = p.Code
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, e
	}
	out := new(nodev1.EnrollResponse)
	if err := proto.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("fakeagent: enrollment response: %w", err)
	}
	return out, nil
}
