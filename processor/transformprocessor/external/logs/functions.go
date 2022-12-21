// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logs // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor/external/logs"

import (
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllogs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor/external/common"
)

func Functions() map[string]interface{} {
	// No logs-only functions yet.
	return common.Functions[ottllogs.TransformContext]()
}
