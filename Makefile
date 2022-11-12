.PHONY: run, test, all

run:
	go run main.go&
	k6 run simple.js