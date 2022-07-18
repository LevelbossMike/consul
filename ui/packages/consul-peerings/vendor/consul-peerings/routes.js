((routes) =>
  routes({
    dc: {
      services: {
        index: {
          _options: {
            queryParams: {
              searchproperty: {
                empty: ["PeerName"],
              },
            },
          },
        },
      },
      nodes: {
        index: {
          _options: {
            queryParams: {
              searchproperty: {
                empty: ["PeerName"],
              },
            },
          },
        },
      },
      peers: {
        _options: {
          path: "/peers",
        },
        index: {
          _options: {
            path: "/",
            queryParams: {
              sortBy: "sort",
              state: "state",
              searchproperty: {
                as: "searchproperty",
                empty: ["Name"],
              },
              search: {
                as: "filter",
                replace: true,
              },
            },
          },
        },
        edit: {
          _options: {
            path: "/:name",
          },
          addresses: {
            _options: {
              path: "/addresses",
            },
          },
        },
      },
    },
  }))(
  (
    json,
    data = typeof document !== "undefined"
      ? document.currentScript.dataset
      : module.exports
  ) => {
    data[`routes`] = JSON.stringify(json);
  }
);
