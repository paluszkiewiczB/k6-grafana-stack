# k6s playground

### TODO

- [X] install k6
- [X] tiny Go app with single endpoint
- [X] k6 script testing the endpoint
- [X] inject some failures
- [X] inject some delays
- [ ] structured logging:
   - [X] ~~logrus?~~ zap
   - [ ] grafana
      - [ ] loki (/w promtail)
      - [ ] dashboard for reading the logs
- [ ] prometheus metrics:
   - [ ] instrument the app
   - [ ] setup prometheus
   - [ ] add dashboard
- [ ] traces
   - [ ] otel with correlation propagation (second endpoint?)
   - [ ] tempo
   - [ ] dashboard
- [ ] monitoring correlations
   - [ ] metrics -> logs -> traces
   - [ ] metrics examplars
- [ ] correlate k6s /w monitoring
   - is it event possible without k6 x Tempo?
   - try prometheus remote write with exemplars on failed checks
      - prometheus does not allow non-chronological writes
      - metrics collected before fail must be sent before the failure metric+exemplar
      - while sending the fail, next metrics must be queued/batched and sent immediately after failure is ack-ed
      - what if there is a lot of failures? drop it or send one-by-one?
