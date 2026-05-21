FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/package-down .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

ENV PORT=3000
EXPOSE 3000

COPY --from=build /out/package-down /usr/local/bin/package-down

ENTRYPOINT ["package-down"]
