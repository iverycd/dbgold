export function partitionPreflightResponse(overrides = {}) {
  const partition = { ready: true, can_start: true, source_type: 'mysql', target_type: 'postgres', issues: [], tables: [], ...overrides }
  return { data: { ready: partition.ready, partition_preflight: partition } }
}
