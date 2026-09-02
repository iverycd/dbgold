const PG_OWNER_DEFAULT_TYPES = new Set([
  'postgres', 'gaussdb', 'seabox', 'highgo', 'vastbase', 'gbase', 'kingbase',
])

// Apply only when the target connection changes, not when its schema changes:
// a user's explicit owner choice must survive schema selection and submission.
export function applyTargetMigrationDefaults(
  state: { lowerCaseNames: boolean; changeOwner: boolean; distributed?: boolean },
  targetType?: string,
) {
  state.lowerCaseNames = targetType !== 'dameng' && targetType !== 'oscar'
  state.changeOwner = PG_OWNER_DEFAULT_TYPES.has(targetType ?? '')
  if (targetType === 'oscar' && 'distributed' in state) state.distributed = false
}

export function defaultBatchOwnerOptions() {
  return { change_owner: true, oscar_change_owner: false }
}
