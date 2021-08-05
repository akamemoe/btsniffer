#!/bin/bash
go mod tidy
go build -o btsniffer -trimpath -ldflags "-s -w -buildid="
