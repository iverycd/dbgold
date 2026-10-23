package damengnative

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ParseBFileLocator preserves the driver's native directory:filename value.
// BFILENAME forbids colons in either component. Its limits are the same byte
// limits enforced by the bundled DM driver's BFILE parameter codec.
func ParseBFileLocator(locator string) (directory, filename string, err error) {
	if !utf8.ValidString(locator) || strings.ContainsRune(locator, 0) || strings.Count(locator, ":") != 1 {
		return "", "", fmt.Errorf("BFILE 定位器必须为完整且有效的目录:文件名")
	}
	directory, filename, _ = strings.Cut(locator, ":")
	if directory == "" || filename == "" || len(directory) > 128 || len(filename) > 256 {
		return "", "", fmt.Errorf("BFILE 目录或文件名为空或超出原生驱动长度范围")
	}
	return directory, filename, nil
}

// CheckBFileResource only verifies a locator's preexisting external resource.
// Reconstructing BFILENAME with separate string bindings avoids the driver's
// unsupported VARCHAR-to-BFILE function argument conversion. Never call the
// package on a NULL locator: DM may evaluate it even inside a SQL CASE branch.
func CheckBFileResource(ctx context.Context, db *sql.DB, locator string) error {
	directory, filename, err := ParseBFileLocator(locator)
	if err != nil {
		return err
	}
	var exists int
	if err = db.QueryRowContext(ctx, `SELECT DBMS_LOB.FILEEXISTS(BFILENAME(?,?)) FROM DUAL`, directory, filename).Scan(&exists); err != nil {
		return fmt.Errorf("BFILE 外部目录、文件或读取权限无法只读核验: %w", err)
	}
	if exists != 1 {
		return fmt.Errorf("BFILE 外部文件不可用，请提前准备目录 %q 中的文件 %q 及其读取权限", directory, filename)
	}
	return nil
}
