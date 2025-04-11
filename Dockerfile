FROM alpine:latest
WORKDIR /app
COPY ./narc-amd64-linux .

ENTRYPOINT [ "/app/narc-amd64-linux" ]