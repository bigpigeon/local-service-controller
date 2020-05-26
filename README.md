### how to run

must in go module 
    # pull all dependency
    go get .
    go build -o local-service-controller
    ./local-service-controller -kubeconfig ~/.kube/config 
    
### create crd and local-service

    kubectl create -f artifacts/examples/crd.yaml
    kubectl create -f artifacts/examples/example.yaml
