include $(GOROOT)/src/Make.inc

TARG=spdy
GOFILES=\
	apipe.go \
	protocol.go \
	server.go \

include $(GOROOT)/src/Make.pkg
