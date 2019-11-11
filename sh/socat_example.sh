#!/bin/bash
socat exec:./connectFoo.sh,pty,stderr,setsid,sigint,sane tcp:localhost:5559

