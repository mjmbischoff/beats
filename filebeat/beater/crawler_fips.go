// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

//go:build requirefips

package beater

import (
	"fmt"

	v2 "github.com/elastic/beats/v7/filebeat/input/v2"
	"github.com/elastic/beats/v7/libbeat/cfgfile"
)

func checkFIPSCapability(runner cfgfile.Runner) error {
	fipsAwareInput, ok := runner.(v2.FIPSAwareInput)
	if !ok {
		// Input is not FIPS-aware; assume it's FIPS capable and proceed
		// without error
		return nil
	}

	if fipsAwareInput.IsFIPSCapable() {
		// Input is FIPS-capable, proceed without error
		return nil
	}

	return fmt.Errorf("running a FIPS-capable distribution but input [%s] is not FIPS capable", runner.String())
}
