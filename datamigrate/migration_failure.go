package datamigrate

import "dbgold/datamigrate/failure"

func isMigrationTaskError(err error) bool { return failure.IsTaskError(err) }
