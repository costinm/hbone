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
	"github.com/costinm/hbone/tcpproxy"
	"github.com/costinm/hbone/otel"
)

// Same as hbone-min, plus OpenCensus instrumentation
// Used for testing size impact of OC as well as perf testing, as client (-L )
func main() {
	auth := hbone.NewAuth()
	hb := hbone.New(auth)

	otel.OTelEnable(hb)

	dest := "127.0.0.1:15008"
	connectorAddr := "127.0.0.1:15009"
	lp := ":12001"
	rp := ":12002"
	if len(os.Args) > 1  {
		dest = os.Args[0]
	}
	go func() {
		tcpproxy.RemoteForwardPort(rp, hb, connectorAddr, "test", "rftest")
	}()
	err := tcpproxy.LocalForwardPort(lp, dest, hb, "", auth)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error forwarding ", err)
		log.Fatal(err)
	}
}
