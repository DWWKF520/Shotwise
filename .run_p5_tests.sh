#!/bin/bash
cd /c/wkf/Shotwise/server-go || exit 9
go test -count=1 -v -run 'TestMigrate' ./internal/service/ > /c/wkf/Shotwise/.gotest_p5.log 2>&1
echo "TEST_EXIT=$?" >> /c/wkf/Shotwise/.gotest_p5.log