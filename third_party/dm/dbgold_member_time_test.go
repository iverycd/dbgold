package dm

import (
	"fmt"
	"testing"
	"time"
)

func temporalMemberDescriptor(kind, scale int32, conn *DmConnection) TypeDescriptor {
	return TypeDescriptor{column: &column{colType: kind, scale: scale}, m_conn: conn, m_serverEncoding: ENCODING_UTF8}
}

func TestDBGoldNativeTemporalMembersKeepWallTimeAndOriginalZone(t *testing.T) {
	for _, zone := range []int{345, -210, 840, -779} {
		for _, kind := range []int32{DATE, TIME, TIME_TZ, DATETIME, DATETIME_TZ, DATETIME2, DATETIME2_TZ} {
			t.Run(fmt.Sprintf("kind_%d_offset_%d", kind, zone), func(t *testing.T) {
				conn := &DmConnection{dmConnector: &DmConnector{localTimezone: 480}, DbTimezone: 0, serverEncoding: ENCODING_UTF8}
				input := time.Date(2026, 1, 1, 0, 15, 56, 123456789, time.FixedZone("original", zone*60))
				desc := temporalMemberDescriptor(kind, 9, conn)
				member, err := TypeDataSV.toMemberObj(input, &desc)
				if err != nil {
					t.Fatal(err)
				}
				got, err := DB2G.toTime(member.m_dataBuf, desc.column, conn)
				if err != nil {
					t.Fatal(err)
				}
				if kind == DATE || kind == DATETIME || kind == DATETIME2 || kind == DATETIME_TZ || kind == DATETIME2_TZ {
					if input.Year() != got.Year() || input.Month() != got.Month() || input.Day() != got.Day() {
						t.Fatalf("native date moved: %s => %s", input.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano))
					}
				}
				if kind != DATE {
					if input.Hour() != got.Hour() || input.Minute() != got.Minute() || input.Second() != got.Second() {
						t.Fatalf("native wall time moved: %s => %s", input.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano))
					}
					wantNsec := input.Nanosecond() / 1000 * 1000
					if kind == DATETIME2 || kind == DATETIME2_TZ {
						wantNsec = input.Nanosecond()
					}
					if got.Nanosecond() != wantNsec {
						t.Fatalf("native fractional second changed: got=%d want=%d", got.Nanosecond(), wantNsec)
					}
				}
				if kind == TIME_TZ || kind == DATETIME_TZ || kind == DATETIME2_TZ {
					_, gotZone := got.Zone()
					if gotZone != zone*60 {
						t.Fatalf("native zone changed: got=%d want=%d", gotZone, zone*60)
					}
				}
			})
		}
	}
}

func TestDBGoldNativeLocalTimezoneMembersRetainNativeConversion(t *testing.T) {
	for _, kind := range []int32{DATETIME, DATETIME2} {
		for _, scaleFlag := range []bool{true, false} {
			conn := &DmConnection{dmConnector: &DmConnector{localTimezone: 480}, DbTimezone: -300, serverEncoding: ENCODING_UTF8}
			scale := int32(6)
			if kind == DATETIME2 {
				scale = 9
			}
			desc := temporalMemberDescriptor(kind, scale, conn)
			if scaleFlag {
				desc.column.scale |= LOCAL_DATETIME_SCALE_MASK
			} else {
				desc.column.mask = MASK_LOCAL_DATETIME
			}
			original := *desc.column
			input := time.Date(2026, 1, 1, 0, 15, 56, 123456789, time.FixedZone("original", 345*60))
			if kind == DATETIME {
				input = input.Truncate(time.Microsecond)
			}
			member, err := TypeDataSV.toMemberObj(input, &desc)
			if err != nil {
				t.Fatal(err)
			}
			wireDT := dmdtDecodeFast(member.m_dataBuf)
			stored := toTimeFromDT(wireDT, int(conn.DbTimezone))
			if !stored.Equal(input) {
				t.Fatalf("LOCAL member lost its instant: input=%s stored=%s", input, stored)
			}
			typed, err := TypeDataSV.convertBytes2BaseData(append([]byte{byte(len(member.m_dataBuf)), 0}, member.m_dataBuf...), newTypeData(nil, nil), &desc)
			if err != nil {
				t.Fatal(err)
			}
			got := typed.m_dumyData.(time.Time)
			_, gotZone := got.Zone()
			if !got.Equal(input) || gotZone != 480*60 {
				t.Fatalf("LOCAL member must decode in session zone without changing instant: input=%s got=%s", input, got)
			}
			if *desc.column != original {
				t.Fatal("member conversion changed immutable wire descriptor mask/scale")
			}
		}
	}
}

func TestDBGoldMemberTimePatchLeavesTopLevelBindingUnchanged(t *testing.T) {
	conn := &DmConnection{dmConnector: &DmConnector{localTimezone: 480}, DbTimezone: 0}
	input := time.Date(2026, 10, 26, 12, 34, 56, 123456000, time.FixedZone("original", 345*60))
	col := column{colType: DATETIME_TZ, scale: 6}
	wire, err := encodeByTime(input, col, *conn)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DB2G.toTime(wire, &col, conn)
	if err != nil {
		t.Fatal(err)
	}
	_, offset := got.Zone()
	if !got.Equal(input) || offset != 480*60 {
		t.Fatalf("ordinary parameter binding behavior changed: %s => %s", input, got)
	}
	invalid := input.In(time.FixedZone("sub-minute", 345*60+1))
	if _, err := encodeNativeMemberTime(invalid, col, conn); err == nil {
		t.Fatal("an unrepresentable native offset must fail instead of truncate")
	}
}
