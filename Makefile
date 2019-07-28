############
# Building #
############

.PHONY: build
build: operator

.PHONY: operator
operator:
	GOOS=linux CGO_ENABLED=0 go build \
	-o $@ cmd/main.go