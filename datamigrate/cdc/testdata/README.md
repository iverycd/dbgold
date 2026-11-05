# CDC integration test

Start the repeatable MySQL/PostgreSQL environment:

Use an exclusive MySQL instance for these tests and run other write tests
sequentially. Separate databases still share the instance-wide binlog, so
concurrent writes can advance the cutover boundary and correctly cause a 409.
Use `-p 1` when testing multiple packages, and do not run another test process
that writes to the same MySQL instance.

```bash
docker compose -f datamigrate/cdc/testdata/docker-compose.yml up -d --wait
CDC_INTEGRATION=1 go test -p 1 -tags=integration ./datamigrate/cdc -run TestCDCIntegration -v
CDC_INTEGRATION=1 go test -p 1 -tags=integration ./api/handler -run TestIncrementalBootstrapJournalIntegration -v
docker compose -f datamigrate/cdc/testdata/docker-compose.yml down -v
```

The test covers transactional INSERT/UPDATE/DELETE, rollback, no-primary-key warning/skip behavior, target checkpoint resume, cutover-boundary drain, DDL pause, acknowledgement, and metadata refresh.
The handler integration test additionally covers durable full-snapshot logs for 100+ tables, a two-page COPY, row-count validation, and bootstrap review after an incompatible table DDL.

MySQL → Dameng also has disposable-instance tests covering its target SQL,
identity handling, LOB row matching, bootstrap recovery and API lifecycle.
See [MySQL → 达梦增量迁移](../../../docs/mysql-dameng-cdc.md) for environment
variables, supported behavior and the test command.
