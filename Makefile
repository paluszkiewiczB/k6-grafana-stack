.PHONY: run, test, docker, docker-build

docker:
	docker-compose up -d
docker-build:
	docker-compose up --build -d
test:
	k6 run --vus 10 --duration 3s simple.js
run: docker-build test