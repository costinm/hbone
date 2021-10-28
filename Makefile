

.go-build:
	(cd cmd/${NAME} && go build -o ${OUT}/${NAME} .)

size-test:
	NAME=hbone $(make) .go-build
	NAME=hbone-min $(make) .go-build
	NAME=hbone-oc $(make) .go-build
	NAME=hbone-otel $(make) .go-build

