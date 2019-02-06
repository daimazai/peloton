// Copyright (c) 2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package objects

import (
	"fmt"

	"github.com/uber/peloton/storage/cassandra"

	"github.com/uber-go/tally"
)

// Store is the ORM store to use for tests
var testStore *Store

// Create and initialize a store for object tests
func setupTestStore() (*Store, error) {
	return NewCassandraStore(
		cassandra.MigrateForTest(),
		tally.NewTestScope("", map[string]string{}))
}

func init() {
	s, err := setupTestStore()
	if err != nil {
		panic(fmt.Sprintf("Failed to setup test store: %v", err))
	}
	testStore = s
}