FROM alpine:3.20
RUN apk add --no-cache bash curl jq kubectl
COPY reaper.sh /opt/reaper.sh
RUN chmod +x /opt/reaper.sh
USER 65534:65534
ENTRYPOINT ["/opt/reaper.sh"]
