module github.com/costinm/hbone/ext/transport

go 1.18

replace github.com/costinm/hbone => ../..

replace github.com/costinm/hbone/ext/http2 => ../../ext/http2

replace github.com/costinm/hbone/ext/transport => ../../ext/transport

require (
	github.com/costinm/hbone v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.0.0-20220812174116-3211cb980234
)

require golang.org/x/text v0.3.7 // indirect
