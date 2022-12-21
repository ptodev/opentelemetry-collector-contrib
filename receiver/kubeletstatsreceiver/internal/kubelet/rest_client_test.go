// Copyright 2020, OpenTelemetry Authors
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

package kubelet

import (
	"testing"

	"github.com/stretchr/testify/require"

	kube "github.com/open-telemetry/opentelemetry-collector-contrib/external/kubelet"
)

func TestRestClient(t *testing.T) {
	rest := NewRestClient(&fakeClient{})
	resp, _ := rest.StatsSummary()
	require.Equal(t, "/stats/summary", string(resp))
	resp, _ = rest.Pods()
	require.Equal(t, "/pods", string(resp))
}

var _ kube.Client = (*fakeClient)(nil)

type fakeClient struct{}

func (f *fakeClient) Get(path string) ([]byte, error) {
	return []byte(path), nil
}
