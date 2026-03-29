import { parseArgs } from "node:util";
import { DuckDBInstance, DuckDBTimestampValue } from '@duckdb/node-api';
import { join } from "node:path";

const options = {
  method: "GET",
  headers: {
    accept: "application/json",
    Authorization: process.env.FOURSQUARE_API_KEY || "ERROR",
  },
};

async function fetchFourSquareMatch(name: string, ll: string): Promise<any> {
  const fields =
    "fsq_id,name,geocodes,location,categories,chains,related_places,timezone,distance,link,closed_bucket,description,tel,fax,email,website,social_media,verified,hours,hours_popular,rating,stats,popularity,price,menu,date_closed,photos,tips,tastes,features,store_id,venue_reality_bucket";
  const query = `query=${name}&ll=${ll}&fields=${fields}&radius=50&limit=1`;

  const url = `https://api.foursquare.com/v3/places/search?${query}`;
  const encodedUrl = encodeURI(url);
  try {
    const res = await fetch(encodedUrl, options);
    const data = await res.json();
    if (data.error) {
      console.error("Error fetching data:", data.error);
      throw data.error;
    }
    
    if (!data.results || data.results.length === 0) {
      console.log(`No results found for ${name}`);
      return {};
    }

    return data.results[0];
  } catch (error) {
    console.error("Error fetching data:", error);
    throw error;
  }
}

async function setupDb(dbPath: string){
  const db = await DuckDBInstance.create(join(process.cwd(), dbPath));
  const conn = await db.connect();
  await conn.run(`INSTALL arrow; LOAD arrow;`);
  await conn.run(`CREATE TABLE IF NOT EXISTS foursquare_data (id VARCHAR PRIMARY KEY, foursquare_data VARCHAR);`)

  return conn;
}

(async () => {
  const {
    values: { database, neighborhood },
  } = parseArgs({
    options: {
      database: {
        type: "string",
        short: "d",
      },
      neighborhood: {
        type: "string",
        short: "n",
      },
    },
  });
  if (!database || !neighborhood) {
    console.error("Please provide a database path and neighborhood");
    process.exit(1);
  }
  let conn;
  try {
    conn = await setupDb(database);

    const reader = await conn.runAndReadAll(
      `select "ID","DOING BUSINESS AS NAME" as dba, LOCATION as location from lic_with_neighborhood where PRI_NEIGH='${neighborhood}';`
    );

    const appender = await conn.createAppender('main', 'foursquare_data');

    const rows: {
      ID: string,
      location: string,
      dba:string,
    }[] = reader.getRowObjects();
    let processed = 0;
    
    for (const row of rows) {
      console.log(JSON.stringify(row, null, 2));

      const foursquareData = await fetchFourSquareMatch(row.dba, row.location.replace('(','').replace(')','').replace(', ', ','));
      //console.log(JSON.stringify(foursquareData, null, 2));

      appender.appendVarchar(row.ID);
      appender.appendVarchar(JSON.stringify(foursquareData));

      appender.endRow();

      if(processed %10){
        appender.flush();
      }
      processed ++;
    } 
  } catch(err){
    console.error('ERROR', err);
  }
  finally {
    if(conn && conn.close){
      await conn.close();
    }
  }
  
})();
