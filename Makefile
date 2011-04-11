include $(GOROOT)/src/Make.inc

TARG=spdy
GOFILES=\
	protocol.go \
	server.go \

include $(GOROOT)/src/Make.pkg
