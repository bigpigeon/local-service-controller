#!/bin/bash
CGO_ENABLED=0 go build
DOCKERIMAGE=bigpigeon0/local-service-app
docker build -t $DOCKERIMAGE .
docker push $DOCKERIMAGE