package dm

import "time"

// Descriptor checksums keep the server's original scale bits. Normalize a copy
// only while encoding/decoding a member, as result-column parsing does.
func nativeMemberColumn(col column) column {
	if isLocalTimeZone(int(col.colType), int(col.scale)) {
		col.scale &^= LOCAL_DATETIME_SCALE_MASK
		col.mask = MASK_LOCAL_DATETIME
	}
	return col
}

// A stored UDT member is already expressed in its native wall time and offset.
// Do not route ordinary or WITH TIME ZONE members through the connection's
// local timezone. WITH LOCAL TIME ZONE still converts to the database timezone
// through the existing mask path.
func encodeNativeMemberTime(value time.Time, col column, conn *DmConnection) ([]byte, error) {
	if conn == nil || conn.dmConnector == nil {
		return nil, ECGO_INVALID_CONN.throw()
	}
	dt := toDTFromTime(value)
	_, offset := value.Zone()
	if offset%60 != 0 || offset/60 <= -13*60 || offset/60 > 14*60 {
		return nil, ECGO_INVALID_DATETIME_FORMAT.throw()
	}
	local := offset / 60
	if col.colType == DATETIME2_TZ {
		// The upstream DATETIME2_TZ encoder writes its zone over the ninth
		// nanosecond byte. Encode the native nine-byte timestamp first, then
		// append its two-byte zone at the offset used by the existing decoder.
		plain := col
		plain.colType = DATETIME2
		data, err := encode(dt, plain, local, int(conn.DbTimezone))
		if err != nil {
			return nil, err
		}
		data = append(data, 0, 0)
		Dm_build_943.Dm_build_954(data, DATETIME2_PREC, int16(local))
		return data, nil
	}
	return encode(dt, col, local, int(conn.DbTimezone))
}
