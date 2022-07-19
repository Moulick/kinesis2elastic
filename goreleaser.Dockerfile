FROM scratch
LABEL org.opencontainers.image.authors=moulickaggarwal
COPY kinesis2elastic /
ENTRYPOINT ["/kinesis2elastic"]
