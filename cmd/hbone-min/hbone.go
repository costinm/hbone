//Copyright 2021 Google LLC
//
//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

package main

import (
	"fmt"
	"log"
	"os"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/auth"
	"github.com/costinm/hbone/ext/tcpproxy"
)

// Redirect stdin/stdout to a TCP-over-Http2 destination, minimal auth.
// This command is intended to find the binary size, compared with an empty go binary and
// variants with more dependencies.
func main() {
	auth := auth.NewMeshAuth()
	hb := hbone.New(auth)

	dest := os.Args[0]
	err := tcpproxy.Forward(dest, hb, "", auth, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error forwarding ", err)
		log.Fatal(err)
	}
}
