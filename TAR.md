# Using az with tar

`.az` files are native LZ4 (levels 1–2) or Zstandard (levels 3–5) streams.
Both `bsdtar` (macOS) and GNU tar detect the format from magic bytes, so no
compression flag is required on extraction.

## Create

```sh
# zstd (default level 3)
tar cf - ./dir | az -c > archive.tar.az

# choose a level explicitly
tar cf - ./dir | az -c -1 > archive-lz4.tar.az   # lz4, fastest
tar cf - ./dir | az -c -5 > archive-zstd.tar.az   # zstd, best ratio
```

## Extract (autodetect)

```sh
tar xf archive.tar.az -C /path/to/dest/
```

`tar` reads the magic bytes and selects the right decompressor automatically —
no `-J`, `--zstd`, or `--lz4` flag needed.

## Verify the format

```sh
xxd archive.tar.az | head -1
# lz4  (levels 1–2):  04 22 4d 18 ...
# zstd (levels 3–5):  28 b5 2f fd ...
```

## Cross-tool decompression

Because the format is standard, any lz4 or zstd tool can decompress `.az` files:

```sh
# level 1–2
lz4  -d archive.tar.az --stdout | tar xf -

# level 3–5
zstd -d archive.tar.az --stdout | tar xf -
```
