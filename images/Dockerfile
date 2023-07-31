ARG BASE_YUM_REPO=release
ARG BASE_OSG_SERIES=3.6

FROM opensciencegrid/software-base:$BASE_OSG_SERIES-el7-$BASE_YUM_REPO

# # Install dependencies
RUN yum -y update \
    && yum -y install golang \
    && yum clean all \
    && rm -rf /var/cache/yum/

WORKDIR /pelican

COPY pelican /pelican
COPY supervisord/supervisord.conf /etc/supervisord.conf
COPY supervisord/pelican_director_serve.conf /etc/supervisord.d/pelican_director_serve.conf
COPY entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh \
    && chmod +x /pelican/pelican \
    && export PATH=/pelican:$PATH

ENTRYPOINT ["/entrypoint.sh"]

