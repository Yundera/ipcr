# A one-page web server: busybox httpd on port 8080, as an unprivileged user.
FROM busybox:1.37
COPY index.html /www/index.html
USER 1000:1000
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
