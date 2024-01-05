module github.com/costinm/hbone

go 1.21

toolchain go1.21.3

replace github.com/costinm/ugate/nio => ../ugate/nio

require (
	github.com/costinm/ugate/nio v0.0.0-00010101000000-000000000000
	golang.org/x/exp v0.0.0-20240103183307-be819d1f06fc
)
