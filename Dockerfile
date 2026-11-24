FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSAO=dev
RUN CGO_ENABLED=0 go test ./internal/sessao ./internal/maquinas ./internal/mcpsrv ./internal/rd/crypto ./internal/rd/wire \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.versao=${VERSAO}" -o /saida/rdmcp ./cmd/rdmcp && mkdir -p /saida/dados

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /saida/rdmcp /rdmcp
COPY --from=build --chown=65532:65532 /saida/dados /dados
VOLUME /dados
ENV RDMCP_OUVIR=0.0.0.0:8090
EXPOSE 8090
ENTRYPOINT ["/rdmcp"]
CMD ["http"]
