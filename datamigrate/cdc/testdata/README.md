# CDC integration test

Start the repeatable MySQL/PostgreSQL environment:

```bash
docker compose -f datamigrate/cdc/testdata/docker-compose.yml up -d --wait
CDC_INTEGRATION=1 go test -tags=integration ./datamigrate/cdc -run TestCDCIntegration -v
docker compose -f datamigrate/cdc/testdata/docker-compose.yml down -v
```

The test covers transactional INSERT/UPDATE/DELETE, rollback, no-primary-key warning/skip behavior, target checkpoint resume, cutover-boundary drain, DDL pause, acknowledgement, and metadata refresh.
