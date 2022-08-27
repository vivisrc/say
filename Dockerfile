FROM golang:1.19 AS builder

WORKDIR /usr/src/say

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
RUN go build -v -o /usr/local/bin/say github.com/vivisrc/say/cmd/bot

FROM debian:stable-slim

RUN apt update && apt -y install ffmpeg ca-certificates
COPY --from=builder /usr/local/bin/say /bin/say

ENTRYPOINT [ "/bin/say" ]
CMD [ ]
