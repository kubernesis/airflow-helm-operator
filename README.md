# airflow-helm-operator
An experiment in distributing and managing airflow via a hybrid helm operator derived from the official helm chart from https://airflow.apache.org/

# example yaml for a minimal install
```
apiVersion: workflow.apache.org/v1alpha1
kind: AirFlow
metadata:
  name: airflow-helm
spec: {
}
```
# accessing the ui
The UI can be accessed via the airflow-helm-webserver. Typically you would expose this via a loadbalancer, however, if testing locally in something like KIND you can reach the ui at localhost:8080 by doing something like the following
```
kubectl port-forward service/airflow-helm-webserver 8080
```
