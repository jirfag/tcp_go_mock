# About
Tcp mock written in Golang for testing of TCP services. In can be used from any testing language/framework.

# Filters
## How they work
Mock behaviour is set by filters. Each filter is represented as
```
type Filter interface {
    // Process full request data.
    processRequest(c *FilterContext) FilterResult

    // Process full response data.
    processResponse(c *FilterContext) FilterResult
}
```

There is global list of filters.

When we have new connection, we run filters on it in infinite loop in two stages:
* ```processRequest``` callbacks of filters are executed in order from left to right, in this stage each filter transforms request.
* ```processResponse``` callbacks are executed in reverse order, in this stage each filter transforms response.

## Builtin filters
* TcpRwFilter - this filter reads until EOF from TCP connection and writes response back. Should be the first filter.
* FileDumperFilter - dump requests to defined file with prefix from sequential number of request: fname.0, fname.1, ...
* ConstResponseFilter - respond with constant string, whic is encoed as HEX to be able to send binary response.
* IprotoWrapperFilter - unpack and pack iproto requests/responses.
* DummyFilter - simple filter, which does nothing. Needed just for saving typing when some filter doesn't have one of callbacks.

# How to control mock
Mock listens for commands on control unix socket (/tmp/tcp_mock.sock by default). Each command should be in new line.
For example, here we control mock to read TCP request until EOF, then dump it to file ```/tmp/mock.txt```, then send response ```hi``` and then close connection:
```
echo "Filters.clear" | nc -U /tmp/tcp_mock.sock
echo "Filters.add TcpRw" | nc -U /tmp/tcp_mock.sock
echo "Filters.add FileDumper /tmp/mock.txt" | nc -U /tmp/tcp_mock.sock
echo "Filters.add ConstResponse 6869" | nc -U /tmp/tcp_mock.sock
```
