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
