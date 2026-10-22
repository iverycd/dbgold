package mysqltrigger

import (
	"errors"
	"fmt"
)

// ErrSourceMetadataRequired identifies conversions blocked by a missing source
// character or numeric-format snapshot, rather than unsupported trigger syntax.
var ErrSourceMetadataRequired = errors.New("触发器转换缺少所需源元数据")

type sourceMetadataRequiredError struct{ message string }

func (e sourceMetadataRequiredError) Error() string { return e.message }
func (e sourceMetadataRequiredError) Unwrap() error { return ErrSourceMetadataRequired }

func sourceMetadataRequired(format string, args ...any) error {
	return sourceMetadataRequiredError{message: fmt.Sprintf(format, args...)}
}
