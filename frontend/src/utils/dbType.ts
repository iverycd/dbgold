const DB_TYPE_CONFIG: Record<string, { color: string; label: string }> = {
  mysql:         { color: 'orange',     label: 'MySQL' },
  postgres:      { color: 'blue',       label: 'PostgreSQL' },
  oracle:        { color: 'red',        label: 'Oracle' },
  sqlserver:     { color: 'cyan',       label: 'SQL Server' },
  gaussdb:       { color: 'green',      label: 'GaussDB' },
  dameng:        { color: 'purple',     label: '达梦' },
  seabox:        { color: 'arcoblue',   label: 'SeaBox' },
  mongodb:       { color: 'lime',       label: 'MongoDB' },
  redis:         { color: 'magenta',    label: 'Redis' },
  elasticsearch: { color: 'gold',       label: 'Elasticsearch' },
  clickhouse:    { color: 'orangered',  label: 'ClickHouse' },
  tidb:          { color: 'pinkpurple', label: 'TiDB' },
  mariadb:       { color: 'orange',     label: 'MariaDB' },
  sqlite:        { color: 'gray',       label: 'SQLite' },
  cassandra:     { color: 'cyan',       label: 'Cassandra' },
  hive:          { color: 'gold',       label: 'Hive' },
  db2:           { color: 'blue',       label: 'DB2' },
  sybase:        { color: 'red',        label: 'Sybase' },
  informix:      { color: 'green',      label: 'Informix' },
  teradata:      { color: 'arcoblue',   label: 'Teradata' },
}

export function getDbTypeColor(dbType: string): string {
  return DB_TYPE_CONFIG[dbType]?.color ?? 'gray'
}

export function getDbTypeLabel(dbType: string): string {
  return DB_TYPE_CONFIG[dbType]?.label ?? dbType
}
