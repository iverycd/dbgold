const PG_OWNER_DEFAULT_TYPES = new Set([
  'postgres', 'gaussdb', 'seabox', 'highgo', 'vastbase', 'gbase', 'kingbase',
])

// Apply only when the target connection changes, not when its schema changes:
// a user's explicit owner choice must survive schema selection and submission.
export function applyTargetMigrationDefaults(
  state: { lowerCaseNames: boolean; changeOwner: boolean; distributed?: boolean; charInLength?: boolean; useNvarchar2?: boolean },
  targetType?: string,
) {
  state.lowerCaseNames = targetType !== 'dameng' && targetType !== 'oscar' && targetType !== 'oracle'
  state.changeOwner = PG_OWNER_DEFAULT_TYPES.has(targetType ?? '')
  if (targetType === 'sqlserver') {
    if ('charInLength' in state) state.charInLength = false
    if ('useNvarchar2' in state) state.useNvarchar2 = false
  }
  if ((targetType === 'oscar' || targetType === 'oracle' || targetType === 'sqlserver') && 'distributed' in state) state.distributed = false
}

export function defaultBatchOwnerOptions() {
  return { change_owner: true, oscar_change_owner: false }
}
