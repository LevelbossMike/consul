import Adapter from './application';

// TODO: Update to use this.formatDatacenter()

// Node and Namespaces are a little strange in that Nodes don't belong in a
// namespace whereas things that belong to a Node do (Health Checks and
// Service Instances). So even though Nodes themselves don't require a
// namespace filter, you sill needs to pass the namespace through to API
// requests in order to get the correct information for the things that belong
// to the node.

export default class NodeAdapter extends Adapter {
  requestForQuery(request, { dc, ns, partition, index, id, uri, withPeers }) {
    if (withPeers) {
      return request`
      GET /v1/internal/ui/nodes?${{ dc }}&with-imports=true
      X-Request-ID: ${uri}

      ${{
        ns,
        partition,
        index,
      }}
    `;
    } else {
      return request`
      GET /v1/internal/ui/nodes?${{ dc }}
      X-Request-ID: ${uri}

      ${{
        ns,
        partition,
        index,
      }}
    `;
    }
  }

  requestForQueryRecord(request, { dc, ns, partition, index, id, uri, peer }) {
    if (typeof id === 'undefined') {
      throw new Error('You must specify an id');
    }
    if (peer) {
      return request`
        GET /v1/internal/ui/node/${id}?${{ dc }}&peer=${peer}
        X-Request-ID: ${uri}

        ${{
          ns,
          partition,
          index,
        }}
      `;
    } else {
      return request`
        GET /v1/internal/ui/node/${id}?${{ dc }}
        X-Request-ID: ${uri}

        ${{
          ns,
          partition,
          index,
        }}
      `;
    }
  }

  // this does not require a partition parameter
  requestForQueryLeader(request, { dc, uri }) {
    return request`
      GET /v1/status/leader?${{ dc }}
      X-Request-ID: ${uri}
      Refresh: 30
    `;
  }

  queryLeader(store, type, id, snapshot) {
    return this.rpc(
      function(adapter, request, serialized, unserialized) {
        return adapter.requestForQueryLeader(request, serialized, unserialized);
      },
      function(serializer, respond, serialized, unserialized) {
        return serializer.respondForQueryLeader(respond, serialized, unserialized);
      },
      snapshot,
      type.modelName
    );
  }
}
