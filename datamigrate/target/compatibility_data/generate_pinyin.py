#!/usr/bin/env python3
"""Generate the pinned Unihan initial table; ordinary builds never run this.

Running without --check downloads the fixed source archive, verifies its SHA256,
and writes only the generated mapping and metadata beside this script.
--check validates the checked-in assets without network access. Supply
--source-zip to additionally reproduce the mapping from a local source archive.
"""

import argparse
import hashlib
import io
import json
from pathlib import Path
import unicodedata
import urllib.request
import zipfile


SOURCE_URL = "https://www.unicode.org/Public/17.0.0/ucd/Unihan.zip"
SOURCE_SHA256 = "f7a48b2b545acfaa77b2d607ae28747404ce02baefee16396c5d2d7a8ef34b5e"
MAPPING_SHA256 = "7095903cd3bb7239e9b1c9394a5b5a8a76d3e4c08d77008d292d720ecd7ad103"
FIRST = 0x3400
LAST = 0x32389
MAPPED_COUNT = 44348
DIRECTORY = Path(__file__).resolve().parent
MAPPING_PATH = DIRECTORY / "pinyin_initials_17_0.txt"
METADATA_PATH = DIRECTORY / "pinyin_metadata.json"


def metadata():
    return {
        "unicode_version": "17.0.0",
        "source_url": SOURCE_URL,
        "source_sha256": SOURCE_SHA256,
        "property": "kMandarin",
        "reading": "first value (zh-Hans); first NFD letter without tone marks",
        "first_codepoint": FIRST,
        "last_codepoint": LAST,
        "unmapped_marker": "?",
        "mapped_count": MAPPED_COUNT,
        "mapping_bytes": LAST - FIRST + 1,
        "mapping_sha256": MAPPING_SHA256,
        "license": "LICENSE-UNICODE.txt",
        "specification_url": "https://www.unicode.org/reports/tr38/tr38-39.html#kMandarin",
    }


def validate(mapping):
    assert len(mapping) == LAST - FIRST + 1, "mapping length changed"
    assert hashlib.sha256(mapping).hexdigest() == MAPPING_SHA256, "mapping SHA256 mismatch"
    assert set(mapping) <= set(b"?abcdefghijklmnopqrstuvwxyz"), "non-ASCII initial"
    assert sum(value != ord("?") for value in mapping) == MAPPED_COUNT, "entry count changed"


def generate(archive):
    assert hashlib.sha256(archive).hexdigest() == SOURCE_SHA256, "source ZIP SHA256 mismatch"
    mapping = bytearray(b"?" * (LAST - FIRST + 1))
    count = 0
    with zipfile.ZipFile(io.BytesIO(archive)) as source:
        # Property grouping may change in later releases; inspect every data file.
        for name in sorted(source.namelist()):
            if not name.startswith("Unihan_") or not name.endswith(".txt"):
                continue
            for line in source.read(name).decode("utf-8").splitlines():
                if not line or line.startswith("#"):
                    continue
                codepoint, prop, value = line.split("\t", 2)
                if prop != "kMandarin":
                    continue
                index = int(codepoint[2:], 16) - FIRST
                assert 0 <= index < len(mapping), "codepoint outside pinned range"
                assert mapping[index] == ord("?"), "duplicate kMandarin property"
                initial = unicodedata.normalize("NFD", value.split()[0])[0]
                assert "a" <= initial <= "z", "unexpected initial"
                mapping[index] = ord(initial)
                count += 1
    assert count == MAPPED_COUNT, "source entry count changed"
    validate(mapping)
    return bytes(mapping)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="validate committed outputs offline")
    parser.add_argument("--source-zip", type=Path, help="read the pinned source archive locally")
    args = parser.parse_args()
    expected_metadata = (json.dumps(metadata(), indent=2, ensure_ascii=True) + "\n").encode("ascii")
    if args.check:
        mapping = MAPPING_PATH.read_bytes()
        validate(mapping)
        assert METADATA_PATH.read_bytes() == expected_metadata, "metadata mismatch"
        if args.source_zip:
            assert generate(args.source_zip.read_bytes()) == mapping, "regenerated mapping differs"
        print("Pinned Unicode 17.0.0 pinyin assets verified (44,348 mappings).")
        return
    archive = args.source_zip.read_bytes() if args.source_zip else urllib.request.urlopen(SOURCE_URL, timeout=60).read()
    MAPPING_PATH.write_bytes(generate(archive))
    METADATA_PATH.write_bytes(expected_metadata)
    print("Generated pinned Unicode 17.0.0 pinyin assets (44,348 mappings).")


if __name__ == "__main__":
    main()
