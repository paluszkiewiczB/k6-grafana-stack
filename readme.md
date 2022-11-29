# k6s playground

### TODO

- [X] install k6
- [X] tiny Go app with single endpoint
- [X] k6 script testing the endpoint
- [X] inject some failures
- [X] inject some delays
- [X] structured logging:
    - [X] ~~logrus?~~ zap
    - [X] grafana
        - [X] app running in docker
        - [X] loki (/w promtail)
        - [X] dashboard for reading the logs
- [X] prometheus metrics:
    - [X] instrument the app
    - [X] setup prometheus
    - [X] add dashboard
- [X] traces
    - [X] otel with correlation propagation (second endpoint?) -> /unstable calls /stable
    - [X] tempo
    - [X] ~~dashboard~~ -> slow traces panel
- [ ] monitoring correlations
    - [X] traces -> logs
    - [X] logs -> traces
    - [ ] metrics -> logs
    - [ ] logs -> metrics
    - [X] metrics -> traces
    - [ ] traces -> metrics
- [ ] correlate k6s /w monitoring
    - is it event possible without k6 x Tempo?
    - try prometheus remote write with exemplars on failed checks
        - prometheus does not allow non-chronological writes
        - metrics collected before fail must be sent before the failure metric+exemplar
        - while sending the fail, next metrics must be queued/batched and sent immediately after failure is ack-ed
        - what if there is a lot of failures? drop it or send one-by-one?
