// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package remotetapprocessor

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestREADMEDocumentsConventions fails when conventions.go and the README
// drift apart, so people and agents always read the real format.
func TestREADMEDocumentsConventions(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	for _, name := range []string{
		AttrSessionID, AttrInstance, AttrProcessor, AttrResourceBytes, AttrSchemaURL,
		AttrDetail, AttrRules, AttrAttributeCount,
		AttrSpanBytes, AttrLogBytes, AttrPointBytes,
		AttrEventCount, AttrLinkCount, AttrBodyBytes,
		DetailSizes, DetailKeys, DetailFull,
	} {
		assert.Contains(t, string(readme), "`"+name+"`", "README.md doesn't document %s", name)
	}
}
