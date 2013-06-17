// Copyright 2013 Tumblr, Inc.
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

package transport

import "circuit/use/circuit"

var (
	ErrEnd           = circuit.NewError("end")
	ErrAlreadyClosed = circuit.NewError("already closed")
	errCollision     = circuit.NewError("conn id collision")
	ErrNotSupported  = circuit.NewError("not supported")
	ErrAuth          = circuit.NewError("authentication")
)