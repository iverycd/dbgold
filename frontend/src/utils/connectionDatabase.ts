const POSTGRES_DATABASE_TYPES = new Set([
  'postgres', 'gaussdb', 'highgo', 'vastbase', 'gbase', 'seabox', 'kingbase',
])

export function isPostgresDatabase(dbType: string): boolean {
  return POSTGRES_DATABASE_TYPES.has(dbType)
}

export function supportsDatabaseEditing(dbType: string): boolean {
  return dbType === 'mysql' || dbType === 'sqlserver' || dbType === 'oscar' || isPostgresDatabase(dbType)
}
