#!/bin/bash

echo "Compiling ..."
CGO_ENABLED=0 go build -a . &
PID=$!
i=1
sp="/-\|"
echo -n ' '
while [ -d /proc/$PID ]
do
  sleep 0.1
  printf "\b${sp:i++%${#sp}:1}"
done
echo ""
echo "Compiled successfully"
echo "Compiling for arm"

while getopts ":t:p:" opt; do
    case $opt in
        t) TAG="$OPTARG"
        ;;
        \?) echo "Invalid option -$OPTARG" >&2
        ;;
    esac
done
if [ ! -z "$TAG" ]; then
    docker build -t appf/controller-conviron:$TAG .
    docker push appf/controller-conviron:$TAG
else
    docker build -t appf/controller-conviron .
fi
