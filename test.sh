#!/bin/bash

set -ex; set -o pipefail

rm log.txt
go run main.go &>log.txt &
k6 run --vus 10 --duration 3s simple.js
#kill process exposing port 8080
netstat -tulpn | grep 8080  | awk '{print$7}' | cut -d'/' -f1 | xargs kill
