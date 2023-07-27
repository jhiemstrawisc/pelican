ARG BASE_YUM_REPO=release
ARG BASE_OSG_SERIES=3.6

FROM opensciencegrid/software-base:$BASE_OSG_SERIES-el7-$BASE_YUM_REPO




# # Install dependencies
RUN yum -y update && yum -y install golang 

#####################
# Workflow Idea     #
#####################
# Whenever something is merged into main in the Pelican repo,
# several git actions, one of which is a "release" action that
# runs the goreleaser to create architecture-specific binaries

# The idea with this Dockerfile is to grab the binary released
# for linux/amd64 and to use that as the Pelican application in
# the produced container.


# # Set the working directory inside the container
# WORKDIR /app
# # Build the Go application
# RUN go build -o pelican .

# # Copy the Supervisor configuration file and entry point script into the container
# COPY supervisord.conf /etc/supervisord.conf
# COPY entrypoint.sh /entrypoint.sh

# # Set the entry point to the script that will manage Supervisor and the "pelican" commands
# ENTRYPOINT ["/entrypoint.sh"]
