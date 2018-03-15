# taintgobgprib

This code is to reproduce a bug in the current version of GoBGP (1.29) as its RIB entry can be tainted by repetitive GetRib() requests. 

## What it does

Generate a stream of GetRib() requests to a local GoBGP daemon until it receives an incorrect response.

## How to reproduce the bug

- Install [GoBGP](https://github.com/osrg/gobgp) and start the daemon.
```
gobgpd --cpus=2 &
```
- Activate the daemon with an arbitrary asn, router-id, and port number.
```
gobgp global as 65001 router-id 10.0.0.1 listen-port 12345
```
- Add two rib entries like so:
```
gobgp global rib add -a ipv4 172.16.0.0/12 nexthop 10.0.0.1
gobgp global rib add -a ipv4 192.168.0.0/16 nexthop 10.0.0.1
```
- Make sure they're properly installed.
```
gobgp global rib
   Network              Next Hop             AS_PATH              Age        Attrs
*> 172.16.0.0/12        10.0.0.1                                  00:00:03   [{Origin: ?}]
*> 192.168.0.0/16       10.0.0.1                                  00:00:03   [{Origin: ?}]
```
- Start this program with two IP addresses that match the previously installed rib.
```
go run taintrib.go 172.16.0.1 192.168.1.1
```
- Sit back and wait until it stops like so:
```
2018/03/15 15:35:05 ERROR: search request #8318360 [172.16.0.1 192.168.1.1], result [172.16.0.0/12 0.168.0.0/16]
exit status 1
```
- See one of the entries is now tainted.
```
gobgp global rib
   Network              Next Hop             AS_PATH              Age        Attrs
*> 172.16.0.0/12        10.0.0.1                                  00:14:12   [{Origin: ?}]
*> 0.168.0.0/16         10.0.0.1                                  00:14:12   [{Origin: ?}]
```
- The number of requests required to hit the bug varies each time. Sometimes it takes less than 1M, other times it's more than 8M like the above (8318360). 

## Usage notes
- The fan-out logic of this code is pretty much stolen from [balancer.go](https://talks.golang.org/2010/io/balance.go), but it might block with no available workers left, depending on the environment it's run on. 

- When it blocks you see no more debug messages that would otherwise show up every five seconds.

- If it happens, you need to tweak values of the first three constants and/or `--cpus` option, then try again.
