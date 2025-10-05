FROM scratch
COPY bin/helm-secrets .
COPY migrations /migrations
CMD ["/helm-secrets"]