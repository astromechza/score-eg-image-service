# score-eg-image-service

An example service that stores a gallery of images and uses an external service to generate thumbnails.

![screenshot.png](screenshot.png).

This service depends on the thumbnail service from <https://github.com/astromechza/score-eg-thumbnail-service>.

![architecture](architecture.drawio.png)

# Testing with Score

```
$ score-compose init
$ curl https://raw.githubusercontent.com/astromechza/score-eg-thumbnail-service/main/score.yaml > score-thumbnail-service.yaml
$ score-compose generate score-thumbnail-service.yaml score.yaml
$ docker compose up -d
$ score-compose resources get-outputs 'dns.default#image-service.dns' --format "http://{{ .host }}:8080"
```

If you want to build the image from source, use the following adjustment:

```
$ score-compose generate score-thumbnail-service.yaml
$ score-compose generate score.yaml --build main=.
$ docker compose up -d --build
```

# Deploying with Score

```
$ score-k8s init
$ score-k8s generate score-thumbnail-service.yaml score.yaml
$ kubectl apply -f manifests.yaml
```
