// Copyright The OpenTelemetry Authors
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

package internal // import "github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourcedetectionprocessor/external"

import "go.opentelemetry.io/collector/pdata/pcommon"

func NewResource(mp map[string]interface{}) pcommon.Resource {
	res := pcommon.NewResource()
	attr := res.Attributes()
	fillAttributeMap(mp, attr)
	return res
}

func NewAttributeMap(mp map[string]interface{}) pcommon.Map {
	attr := pcommon.NewMap()
	fillAttributeMap(mp, attr)
	return attr
}

func fillAttributeMap(mp map[string]interface{}, attr pcommon.Map) {
	attr.Clear()
	attr.EnsureCapacity(len(mp))
	for k, v := range mp {
		switch t := v.(type) {
		case bool:
			attr.PutBool(k, t)
		case int64:
			attr.PutInt(k, t)
		case float64:
			attr.PutDouble(k, t)
		case string:
			attr.PutStr(k, t)
		}
	}
}
