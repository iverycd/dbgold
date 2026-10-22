# Pinned DM Go driver patches

This directory contains the unmodified files of `gitee.com/chunanyong/dm` v1.8.23
(DM 8.1.4.200, upstream `VERSION` dated 2026-03-11), except the focused changes
listed below. Upstream copyright notices and documentation are retained. Its
source is [the upstream module](https://gitee.com/chunanyong/dm/tree/v1.8.23);
the [DM Go programming guide](https://eco.dameng.com/document/dm/zh-cn/pm/go-rogramming-guide.html)
documents the driver. The root module pins this directory with a `replace`.

1. `o.go`: preserve the complete 20-byte base-100 mantissa of negative decimal
   values. Omit the optional negative terminator when the 21-byte wire field is
   full; reject oversized mantissas instead of silently truncating them. Permit
   negative exponent header 0, already supported by the upstream decoder.
2. `zzo.go`: write a UDT LOB's payload after its four-byte length header instead
   of overwriting the header. Encode CLOB lengths in server-encoded bytes, as
   required by the upstream `bytesToClob` decoder. Propagate LOB encoding errors.
   Dispatch named collections described as `CLASS` object ID 4 through the
   collection encoder when nested inside objects or other collections. The
   ordinary object encoder otherwise drops every collection element.

`dbgold_codec_test.go` exercises the actual driver encoders and existing decoders
without a database, including nested object/array values, negative 38-digit
decimals with odd scale, Unicode/NUL CLOBs, BLOBs, NULL and empty LOBs. Run it with:

```sh
go test ./...
```

Run this command from this directory. The repository's separate gated real-DM
tests validate the server wire roundtrip when dedicated test connections exist.
