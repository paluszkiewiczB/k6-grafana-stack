.PHONY: run, traffic, docker

docker:
	docker-compose up --build -d
traffic:
	k6 run --vus 10 --duration 10s simple.js
run: docker traffic