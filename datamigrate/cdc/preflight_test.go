package cdc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadBinlogRetention(t *testing.T) {
	tests := []struct {
		name      string
		values    map[string]string
		errors    map[string]error
		want      *int64
		wantError []string
	}{
		{
			name:   "mysql 8 seconds",
			values: map[string]string{"binlog_expire_logs_auto_purge": "ON", "binlog_expire_logs_seconds": "2592000"},
			want:   int64Ptr(2592000),
		},
		{
			name:   "mysql 8 falls back to days when seconds is zero",
			values: map[string]string{"binlog_expire_logs_seconds": "0", "expire_logs_days": "7"},
			want:   int64Ptr(7 * 24 * 60 * 60),
		},
		{
			name:   "mysql 57 uses days",
			values: map[string]string{"expire_logs_days": "14"},
			errors: map[string]error{"binlog_expire_logs_seconds": errors.New("unknown system variable")},
			want:   int64Ptr(14 * 24 * 60 * 60),
		},
		{
			name:   "both periods zero disables purge",
			values: map[string]string{"binlog_expire_logs_seconds": "0", "expire_logs_days": "0"},
			want:   int64Ptr(0),
		},
		{
			name:   "mysql 57 zero days disables purge",
			values: map[string]string{"expire_logs_days": "0"},
			errors: map[string]error{"binlog_expire_logs_seconds": errors.New("unknown system variable")},
			want:   int64Ptr(0),
		},
		{
			name:   "mysql 8029 auto purge off wins",
			values: map[string]string{"binlog_expire_logs_auto_purge": "OFF", "binlog_expire_logs_seconds": "2592000", "expire_logs_days": "7"},
			want:   int64Ptr(0),
		},
		{
			name: "unknown when neither period can be read",
			errors: map[string]error{
				"binlog_expire_logs_seconds": errors.New("seconds denied"),
				"expire_logs_days":           errors.New("days denied"),
			},
			wantError: []string{"binlog_expire_logs_seconds", "seconds denied", "expire_logs_days", "days denied"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(_ context.Context, name string) (string, error) {
				if err := tt.errors[name]; err != nil {
					return "", err
				}
				if value, ok := tt.values[name]; ok {
					return value, nil
				}
				return "", errors.New("unknown system variable")
			}

			got, err := readBinlogRetention(context.Background(), lookup)
			if len(tt.wantError) == 0 {
				require.NoError(t, err)
				require.NotNil(t, got)
				require.Equal(t, *tt.want, *got)
				return
			}
			require.Error(t, err)
			require.Nil(t, got)
			for _, fragment := range tt.wantError {
				require.Contains(t, err.Error(), fragment)
			}
		})
	}
}

func TestReadBinlogRetentionRejectsOverflow(t *testing.T) {
	lookup := func(_ context.Context, name string) (string, error) {
		switch name {
		case "binlog_expire_logs_seconds":
			return "0", nil
		case "expire_logs_days":
			return "9223372036854775807", nil
		default:
			return "", errors.New("unknown system variable")
		}
	}

	got, err := readBinlogRetention(context.Background(), lookup)
	require.Nil(t, got)
	require.ErrorContains(t, err, "超出可支持范围")
}

func TestRetentionDurationWarningBoundary(t *testing.T) {
	below := int64(72*60*60 - 1)
	exact := int64(72 * 60 * 60)
	above := exact + 1
	disabled := int64(0)

	require.NotEmpty(t, retentionDurationWarning(&below))
	require.Empty(t, retentionDurationWarning(&exact))
	require.Empty(t, retentionDurationWarning(&above))
	require.Empty(t, retentionDurationWarning(&disabled))
	require.Empty(t, retentionDurationWarning(nil))
}

func TestCompactError(t *testing.T) {
	message := compactError(errors.New("first\n\tsecond " + strings.Repeat("x", 300)))
	require.NotContains(t, message, "\n")
	require.LessOrEqual(t, len([]rune(message)), 241)
}

func TestPreflightRetentionJSONDistinguishesUnknownAndDisabled(t *testing.T) {
	unknown, err := json.Marshal(PreflightResult{})
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":false,"log_bin":false,"binlog_format":"","binlog_row_image":"","gtid_mode":"","binlog_retention_seconds":null,"current_position":{"file":"","position":0,"gtid":""},"tables":null,"no_primary_key_tables":null,"errors":null,"warnings":null}`, string(unknown))

	disabled, err := json.Marshal(PreflightResult{RetentionSecs: int64Ptr(0)})
	require.NoError(t, err)
	require.Contains(t, string(disabled), `"binlog_retention_seconds":0`)
}
