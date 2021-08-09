btsniffer - a sniffer that sniffs torrents from BitTorrent network
======================================

## Introduction

Refactored from [torsniff](https://github.com/fanpei91/torsniff), thanks for the original project author's efforts.
> *Enhancement:* Also sinffs on the BitTorrent network but only stores torrents that hit your keywords.

## Compiling

> Please ensure you have golang installed in your system environment
```bash
./build.sh
```

## Usage

```
$ ./btsniffer -h

Usage:
  torsniff [flags]

Flags:
  -k, --kwfile string      the keywords file (default ./keywords.txt;split by line)
  -o, --database string    the output database, all torrents will be saved to that file (default ./torrentdata.db)
  -a, --addr string        listen on given address (default all, ipv4 and ipv6)
  -d, --dir string         the directory to store the torrents (default "$HOME/torrents")
  -f, --friends int        max fiends to make with per second (default 500)
  -e, --peers int          max peers to connect to download torrents (default 400)
  -p, --port uint16        listen on given port (default 6881)
  -t, --timeout duration   max time allowed for downloading torrents (default 10s)
  -h, --help               help for torsniff
  -v, --verbose            run in verbose mode (default true)
```

## Quick start
Use default flags:

`./btsniffer`


## Protocols References
- [DHT Protocol](http://www.bittorrent.org/beps/bep_0005.html)
- [The BitTorrent Protocol Specification](http://www.bittorrent.org/beps/bep_0003.html)
- [BitTorrent  Extension Protocol](http://www.bittorrent.org/beps/bep_0010.html)
- [Extension for Peers to Send Metadata Files](http://www.bittorrent.org/beps/bep_0009.html)

## License
MIT
